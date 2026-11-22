package server

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

func (config composeSpec) isolateTest(project, source string) error {
	for _, name := range slices.Sorted(maps.Keys(config.services)) {
		service := config.services[name]
		delete(service, "ports")
		if err := validateTestService(name, source, service); err != nil {
			return err
		}
		build := object(service["build"])
		if build != nil {
			delete(service, "image")
			delete(build, "tags")
		}
	}
	if err := config.validate(nil); err != nil {
		return err
	}
	if err := validateTestResources(config.document, project, "network", "networks", "bridge"); err != nil {
		return err
	}
	if err := validateTestResources(config.document, project, "volume", "volumes", "local"); err != nil {
		return err
	}
	if err := validateTestFileResources(config.document, source, "configs"); err != nil {
		return err
	}
	return validateTestFileResources(config.document, source, "secrets")
}

func validateTestService(name, source string, service map[string]any) error {
	network := setting(service["network_mode"])
	if network != "" && network != "none" && !strings.HasPrefix(network, "service:") {
		return fmt.Errorf("service %s must use project networking", name)
	}
	for _, field := range []string{"ipc", "pid", "uts", "cgroup", "userns_mode"} {
		namespace := setting(service[field])
		if namespace == "host" || strings.HasPrefix(namespace, "container:") {
			return fmt.Errorf("service %s cannot share the host or another project's %s namespace", name, field)
		}
	}
	for _, field := range []string{"provider", "privileged", "use_api_socket", "cap_add", "devices", "device_cgroup_rules", "external_links"} {
		if configuredTestValue(service[field]) {
			return fmt.Errorf("service %s cannot use %s in an isolated test", name, field)
		}
	}
	return validateTestServicePaths(name, source, service)
}

func validateTestServicePaths(name, source string, service map[string]any) error {
	for _, raw := range list(service["volumes"]) {
		volume := object(raw)
		if volume["type"] == "bind" && !testPathWithin(source, source, setting(volume["source"])) {
			return fmt.Errorf("service %s has a bind mount outside the test snapshot", name)
		}
	}
	for _, volume := range list(service["volumes_from"]) {
		if strings.HasPrefix(setting(volume), "container:") {
			return fmt.Errorf("service %s cannot mount volumes from an existing container", name)
		}
	}
	return validateTestBuild(name, source, object(service["build"]))
}

func validateTestBuild(name, source string, build map[string]any) error {
	if build == nil {
		return nil
	}
	for _, field := range []string{"privileged", "entitlements", "outputs", "cache_to"} {
		if configuredTestValue(build[field]) {
			return fmt.Errorf("service %s build cannot use %s in an isolated test", name, field)
		}
	}
	if network := setting(build["network"]); network != "" && network != "none" && network != "default" {
		return fmt.Errorf("service %s build must use default build networking", name)
	}
	for _, value := range object(build["additional_contexts"]) {
		context := setting(value)
		if !testPathWithin(source, source, context) && !strings.HasPrefix(context, "service:") && !strings.HasPrefix(context, "docker-image://") {
			return fmt.Errorf("service %s has an additional build context outside the test snapshot", name)
		}
	}

	buildContext := setting(build["context"])
	if buildContext == "" {
		buildContext = "."
	}
	if !testPathWithin(source, source, buildContext) {
		return fmt.Errorf("service %s has a build context outside the test snapshot", name)
	}
	dockerfile := setting(build["dockerfile"])
	if dockerfile != "" && !testPathWithin(source, testAbsolutePath(source, buildContext), dockerfile) {
		return fmt.Errorf("service %s has a Dockerfile outside the test snapshot", name)
	}
	return nil
}

func testPathWithin(source, base, filename string) bool {
	return filename != "" && fileWithin(source, testAbsolutePath(base, filename))
}

func testAbsolutePath(base, filename string) string {
	if filepath.IsAbs(filename) {
		return filename
	}
	return filepath.Join(base, filename)
}

func validateTestResources(document map[string]any, project, kind, section, defaultDriver string) error {
	resources := object(document[section])
	for _, name := range slices.Sorted(maps.Keys(resources)) {
		resource := object(resources[name])
		resourceName := setting(resource["name"])
		if configuredTestValue(resource["external"]) || (resourceName != "" && resourceName != project+"_"+name) {
			return fmt.Errorf("%s %s must use a Compose-generated project name for an isolated test", kind, name)
		}
		driver := setting(resource["driver"])
		if (driver != "" && driver != defaultDriver) || configuredTestValue(resource["driver_opts"]) {
			return fmt.Errorf("%s %s must use the default driver without driver options for an isolated test", kind, name)
		}
	}
	return nil
}

func validateTestFileResources(document map[string]any, source, section string) error {
	resources := object(document[section])
	for _, name := range slices.Sorted(maps.Keys(resources)) {
		resource := object(resources[name])
		if configuredTestValue(resource["external"]) {
			return fmt.Errorf("external resource %s in %s cannot be used in an isolated test", name, section)
		}
		filename := setting(resource["file"])
		if filename != "" && !testPathWithin(source, source, filename) {
			return fmt.Errorf("resource %s in %s has a file outside the test snapshot", name, section)
		}
	}
	return nil
}

func configuredTestValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case []any:
		return len(typed) != 0
	case map[string]any:
		return len(typed) != 0
	default:
		return true
	}
}

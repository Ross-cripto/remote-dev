package project

import (
	"fmt"
	"regexp"
	"strings"
)

const nodeTestService = "test"

var npmInitTestScript = regexp.MustCompile(`^echo[[:space:]]+(?:"Error: no test specified"|'Error: no test specified')[[:space:]]*&&[[:space:]]*exit[[:space:]]+1$`)

func LoadTests(root, host, service string) (Project, string, error) {
	if service != "" {
		if _, err := validateName(service, "test service name"); err != nil {
			return Project{}, "", err
		}
	}
	root, filename, err := projectLocation(root, host)
	if err != nil {
		return Project{}, "", err
	}
	if filename != "" {
		return configuredTests(root, filename, host, service)
	}

	composeFile, override, err := composePaths(root)
	if err != nil {
		return Project{}, "", err
	}
	if composeFile == "" {
		return detectedNodeTests(root, host, service)
	}
	source := composeFile
	if override != "" {
		source += " + " + override
	}
	detected := Project{Mode: "compose", Host: host, ComposeFile: composeFile, Override: override, DetectedFrom: source}
	return detected, service, nil
}

func configuredTests(root, filename, host, service string) (Project, string, error) {
	detected, config, err := configuredProject(root, filename, host)
	if err != nil {
		return Project{}, "", err
	}
	detected.Forwards = nil
	if detected.Mode == "compose" {
		return detected, service, nil
	}

	services, err := configTable(config["services"], "services")
	if err != nil {
		return Project{}, "", err
	}
	service, err = configuredTestServiceName(services, service)
	if err != nil {
		return Project{}, "", err
	}
	serviceConfig, found := services[service]
	if !found {
		if detected.Mode == "node" && (service == "" || service == nodeTestService) {
			return detectedNodeTests(root, detected.Host, service)
		}
		if service == "" {
			return Project{}, "", fmt.Errorf("no test service in %s. Define services.test or choose a service with --service", ConfigFile)
		}
		return Project{}, "", fmt.Errorf("test service %q is not defined in %s", service, ConfigFile)
	}
	if detected.Mode == "node" {
		key := "services." + service
		settings, err := configTable(serviceConfig, key)
		if err != nil {
			return Project{}, "", err
		}
		if _, err := nonemptyString(settings["script"], key+".script"); err != nil {
			return Project{}, "", err
		}
	}

	volumes := make(map[string]struct{})
	testService, err := configuredService(root, detected.Mode, service, serviceConfig, nil, volumes)
	if err != nil {
		return Project{}, "", err
	}
	testService.Labels = nil
	document := composeDocument{Services: map[string]composeService{service: testService}, Volumes: volumes}
	detected, err = withGenerated(detected, document)
	return detected, service, err
}

func configuredTestServiceName(services map[string]any, selected string) (string, error) {
	if selected != "" {
		return selected, nil
	}
	_, hasTest := services[nodeTestService]
	_, hasTests := services["tests"]
	switch {
	case hasTest && hasTests:
		return "", fmt.Errorf("both test and tests are configured in %s. Select one with --service", ConfigFile)
	case hasTest:
		return nodeTestService, nil
	case hasTests:
		return "tests", nil
	default:
		return "", nil
	}
}

func detectedNodeTests(root, host, service string) (Project, string, error) {
	if service != "" && service != nodeTestService {
		return Project{}, "", fmt.Errorf("no test service named %q. A detected Node project exposes the test service", service)
	}
	rootPackage, err := readNodePackage(root, ".")
	if err != nil {
		return Project{}, "", err
	}
	if rootPackage == nil || !hasNodeTestScript(*rootPackage) {
		return Project{}, "", fmt.Errorf("no test service or root package.json test script found. Define a test service in Compose or %s", ConfigFile)
	}
	testService := newComposeService(".", nodeSource, map[string]string{"CI": "true"}, nil)
	testService, err = configureDetectedNodeService(root, *rootPackage, *rootPackage, nodeTestService, nil, testService)
	if err != nil {
		return Project{}, "", err
	}
	detected := Project{Mode: "node", Host: host, DetectedFrom: "package.json"}
	document := composeDocument{Services: map[string]composeService{nodeTestService: testService}}
	detected, err = withGenerated(detected, document)
	return detected, nodeTestService, err
}

func hasNodeTestScript(rootPackage nodePackage) bool {
	script := strings.TrimSpace(rootPackage.script(nodeTestService))
	return script != "" && !strings.ContainsRune(script, 0) && !npmInitTestScript.MatchString(script)
}

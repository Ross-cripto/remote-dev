package project

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

var composeFiles = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

var developmentComposeFiles = []string{
	"compose.dev.yaml", "compose.dev.yml", "compose.development.yaml", "compose.development.yml",
	"docker-compose.dev.yaml", "docker-compose.dev.yml", "docker-compose.development.yaml", "docker-compose.development.yml",
	"compose.override.yaml", "compose.override.yml", "docker-compose.override.yaml", "docker-compose.override.yml",
}

var infrastructureImages = []string{
	"postgres", "redis", "mysql", "mariadb", "mongo", "memcached", "rabbitmq", "minio/minio", "valkey/valkey",
}

var staticImage = regexp.MustCompile(`^([a-z0-9./-]+)(?::[A-Za-z0-9_.-]+)?(?:@sha256:[a-f0-9]{64})?$`)
var composeProperty = regexp.MustCompile(`^[a-z_]+$`)

func detectProject(root, host string) (Project, error) {
	base, override, err := composePaths(root)
	if err != nil {
		return Project{}, err
	}
	compose := Project{Host: host, Mode: "compose", ComposeFile: base, Override: override, AutoPorts: true, DetectedFrom: base}
	if override != "" {
		compose.DetectedFrom += " + " + override
		return compose, nil
	}
	var infrastructureServices map[string]bool
	if base != "" {
		infrastructureServices, err = detectInfrastructure(root, base)
		if err != nil {
			return Project{}, err
		}
		if infrastructureServices == nil {
			return compose, nil
		}
	}

	rootPackage, err := readNodePackage(root, ".")
	if err != nil {
		return Project{}, err
	}
	patterns, err := workspacePatterns(root, rootPackage)
	if err != nil {
		return Project{}, err
	}
	apps, err := detectNodeApps(root, rootPackage, patterns)
	if err != nil {
		return Project{}, err
	}
	if len(apps) == 0 {
		if base != "" {
			return compose, nil
		}
		return Project{}, fmt.Errorf("no supported Compose file or recognizable Node dev script was found. Add %s with mode = 'commands' for a custom launch command", ConfigFile)
	}

	runAtRoot := canRunWorkspaceScript(rootPackage, patterns, apps)
	generated, forwards, err := nodeCompose(root, rootPackage, patterns, apps, runAtRoot)
	if err != nil {
		return Project{}, err
	}
	var collisions []string
	for service := range generated.Services {
		if infrastructureServices[service] {
			collisions = append(collisions, service)
		}
	}
	if len(collisions) > 0 {
		slices.Sort(collisions)
		return Project{}, fmt.Errorf("detected Node services overlap with Compose services (%s). Select development services in %s", strings.Join(collisions, ", "), ConfigFile)
	}
	source := detectionSource(base, apps, runAtRoot)
	project := Project{Mode: "node", Host: host, ComposeFile: base, AutoPorts: base != "", Forwards: forwards, DetectedFrom: source}
	return withGenerated(project, generated)
}

func detectionSource(composeFile string, apps []nodeApp, runAtRoot bool) string {
	var manifests []string
	for _, app := range apps {
		manifests = append(manifests, path.Join(app.Package.Directory, "package.json"))
	}
	source := strings.Join(manifests, ", ")
	if runAtRoot {
		source = fmt.Sprintf("package.json (workspace dev script for %s)", source)
	}
	if composeFile != "" {
		source = composeFile + " + " + source
	}
	return source
}

func composePaths(root string) (string, string, error) {
	var base string
	for _, filename := range composeFiles {
		found, err := projectFile(root, filename)
		if err != nil {
			return "", "", err
		}
		if found {
			base = filename
			break
		}
	}
	var development []string
	for _, filename := range developmentComposeFiles {
		found, err := projectFile(root, filename)
		if err != nil {
			return "", "", err
		}
		if found {
			development = append(development, filename)
		}
	}
	if len(development) > 1 {
		return "", "", fmt.Errorf("several development Compose files were found (%s). Select one with compose_file in %s", strings.Join(development, ", "), ConfigFile)
	}
	if len(development) == 0 {
		return base, "", nil
	}
	if base == "" {
		return development[0], "", nil
	}
	return base, development[0], nil
}

func detectInfrastructure(root, composeFile string) (map[string]bool, error) {
	content, err := os.ReadFile(filepath.Join(root, composeFile))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", composeFile, err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil || len(document.Content) != 1 {
		return nil, nil
	}
	topLevel := document.Content[0]
	if !isYAMLMapping(topLevel) {
		return nil, nil
	}
	var services *yaml.Node
	for index := 0; index < len(topLevel.Content); index += 2 {
		key := topLevel.Content[index].Value
		if !isComposeKey(key) {
			return nil, nil
		}
		if key == "services" {
			services = topLevel.Content[index+1]
		}
	}
	if services == nil || !isYAMLMapping(services) || len(services.Content) == 0 {
		return nil, nil
	}
	names := make(map[string]bool)
	for index := 0; index < len(services.Content); index += 2 {
		name := services.Content[index].Value
		if !projectName.MatchString(name) || !isInfrastructureService(services.Content[index+1]) {
			return nil, nil
		}
		names[name] = true
	}
	return names, nil
}

func isComposeKey(key string) bool {
	switch key {
	case "name", "version", "services", "volumes", "networks", "secrets", "configs":
		return true
	default:
		return strings.HasPrefix(key, "x-")
	}
}

func isYAMLMapping(node *yaml.Node) bool {
	if node.Kind != yaml.MappingNode || node.Tag != "!!map" {
		return false
	}
	seen := make(map[string]bool)
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
			return false
		}
		seen[key.Value] = true
	}
	return true
}

func isInfrastructureService(service *yaml.Node) bool {
	if !isYAMLMapping(service) {
		return false
	}
	var image string
	for index := 0; index < len(service.Content); index += 2 {
		property := service.Content[index].Value
		if !composeProperty.MatchString(property) {
			return false
		}
		switch property {
		case "build", "extends", "entrypoint", "networks", "network_mode", "profiles":
			return false
		case "image":
			value := service.Content[index+1]
			if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
				return false
			}
			image = value.Value
		}
	}
	match := staticImage.FindStringSubmatch(image)
	if match == nil {
		return false
	}
	name := strings.TrimPrefix(strings.TrimPrefix(match[1], "docker.io/"), "library/")
	return slices.Contains(infrastructureImages, name)
}

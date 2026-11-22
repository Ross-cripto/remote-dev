package project

import (
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func loadConfiguration(root, filename, host string) (Project, error) {
	project, config, err := configuredProject(root, filename, host)
	if err != nil || project.Mode == "compose" {
		return project, err
	}
	generated, err := configuredServices(root, project.Mode, config["services"], project.Forwards)
	if err != nil {
		return Project{}, err
	}
	return withGenerated(project, generated)
}

func configuredProject(root, filename, host string) (Project, map[string]any, error) {
	content, err := os.ReadFile(filename)
	if err != nil {
		return Project{}, nil, fmt.Errorf("read %s: %w", filename, err)
	}
	var config map[string]any
	if err := toml.Unmarshal(content, &config); err != nil {
		return Project{}, nil, fmt.Errorf("read %s: %w", filename, err)
	}
	if host == "" {
		host, err = validateName(config["host"], "SSH host alias")
		if err != nil {
			return Project{}, nil, err
		}
	}
	mode, err := validateMode(config["mode"])
	if err != nil {
		return Project{}, nil, err
	}
	modeKey := "services"
	if mode == "compose" {
		modeKey = "compose_file"
	}
	if err := validateKeys(config, ConfigFile, "host", "mode", "forwards", modeKey); err != nil {
		return Project{}, nil, err
	}
	forwards, err := parseForwards(valueOrDefault(config, "forwards", map[string]any{}))
	if err != nil {
		return Project{}, nil, err
	}
	project := Project{Mode: mode, Host: host, Forwards: forwards}
	if mode == "compose" {
		project.ComposeFile, err = configuredComposeFile(root, config)
	}
	return project, config, err
}

func configuredComposeFile(root string, config map[string]any) (string, error) {
	filename, err := relativePath(root, valueOrDefault(config, "compose_file", defaultComposeFile), "compose_file")
	if err != nil {
		return "", err
	}
	if filename == GeneratedComposeFile {
		return "", fmt.Errorf("the compose_file %s is reserved", filename)
	}
	if !isRegularFile(filepath.Join(root, filepath.FromSlash(filename))) {
		return "", fmt.Errorf("Compose file does not exist: %s", filename)
	}
	return filename, nil
}

func validateMode(value any) (string, error) {
	mode, _ := value.(string)
	if _, supported := modeTemplates[mode]; !supported {
		return "", fmt.Errorf("mode must be compose, commands or node")
	}
	return mode, nil
}

func validateName(value any, key string) (string, error) {
	name, ok := value.(string)
	if !ok || !projectName.MatchString(name) {
		return "", fmt.Errorf("%s must start with a letter or number and contain only letters, numbers, dots, underscores or hyphens", key)
	}
	return name, nil
}

func validateKeys(table map[string]any, key string, allowed ...string) error {
	var unexpected []string
	for name := range table {
		if !slices.Contains(allowed, name) {
			unexpected = append(unexpected, name)
		}
	}
	if len(unexpected) == 0 {
		return nil
	}
	slices.Sort(unexpected)
	return fmt.Errorf("unknown key in %s: %s", key, strings.Join(unexpected, ", "))
}

func configTable(value any, key string) (map[string]any, error) {
	table, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a table", key)
	}
	return table, nil
}

func valueOrDefault(table map[string]any, key string, fallback any) any {
	value, found := table[key]
	if found {
		return value
	}
	return fallback
}

func nonemptyString(value any, key string) (string, error) {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" || strings.ContainsRune(text, 0) {
		return "", fmt.Errorf("%s must be a non-empty string without null characters", key)
	}
	return text, nil
}

func relativePath(root string, value any, key string) (string, error) {
	relative, err := nonemptyString(value, key)
	if err != nil {
		return "", err
	}
	if path.IsAbs(relative) || slices.Contains(strings.Split(relative, "/"), "..") {
		return "", fmt.Errorf("%s must be a project-relative path without parent traversal", key)
	}
	for _, character := range relative {
		if !strconv.IsPrint(character) {
			return "", fmt.Errorf("%s must be a project-relative path without parent traversal", key)
		}
	}
	relative = path.Clean(relative)
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(relative)))
	if os.IsNotExist(err) {
		return relative, nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", key, err)
	}
	within, err := filepath.Rel(root, resolved)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s must point inside the project", key)
	}
	return relative, nil
}

func isRegularFile(filename string) bool {
	info, err := os.Stat(filename)
	return err == nil && info.Mode().IsRegular()
}

func validTCPPort(port int64) bool {
	return port >= 1 && port <= maxTCPPort
}

func configPort(value any, key string) (int, error) {
	port, ok := value.(int64)
	if !ok || !validTCPPort(port) {
		return 0, fmt.Errorf("%s must be an integer from 1 to %d", key, maxTCPPort)
	}
	return int(port), nil
}

func parseForwards(value any) ([]Forward, error) {
	table, err := configTable(value, "forwards")
	if err != nil {
		return nil, err
	}
	var forwards []Forward
	localPorts := map[int]bool{}
	for _, name := range slices.Sorted(maps.Keys(table)) {
		forward, err := parseForward(name, table[name])
		if err != nil {
			return nil, err
		}
		if forward.Local != nil {
			if localPorts[*forward.Local] {
				return nil, fmt.Errorf("local port %d is used by more than one forward", *forward.Local)
			}
			localPorts[*forward.Local] = true
		}
		forwards = append(forwards, forward)
	}
	return forwards, nil
}

func parseForward(name string, value any) (Forward, error) {
	key := "forwards." + name
	if _, err := validateName(name, "forward name"); err != nil {
		return Forward{}, err
	}
	config, err := configTable(value, key)
	if err != nil {
		return Forward{}, err
	}
	if err := validateKeys(config, key, "service", "target", "local"); err != nil {
		return Forward{}, err
	}
	service, err := validateName(config["service"], key+".service")
	if err != nil {
		return Forward{}, err
	}
	target, err := configPort(config["target"], key+".target")
	if err != nil {
		return Forward{}, err
	}
	forward := Forward{Name: name, Service: service, Target: target}
	local := valueOrDefault(config, "local", "auto")
	if text, ok := local.(string); ok && text == "auto" {
		return forward, nil
	}
	port, err := configPort(local, key+".local")
	if err != nil {
		return Forward{}, err
	}
	forward.Local = &port
	return forward, nil
}

func configuredServices(root, mode string, value any, forwards []Forward) (composeDocument, error) {
	configs, err := configTable(value, "services")
	if err != nil {
		return composeDocument{}, err
	}
	if len(configs) == 0 {
		return composeDocument{}, fmt.Errorf("configure at least one service for %s mode", mode)
	}
	targets := make(map[string][]int)
	for _, forward := range forwards {
		if _, exists := configs[forward.Service]; !exists {
			return composeDocument{}, fmt.Errorf("forward %s references unknown service %s", forward.Name, forward.Service)
		}
		targets[forward.Service] = append(targets[forward.Service], forward.Target)
	}
	document := composeDocument{Services: make(map[string]composeService), Volumes: make(map[string]struct{})}
	for _, name := range slices.Sorted(maps.Keys(configs)) {
		ports := targets[name]
		slices.Sort(ports)
		service, err := configuredService(root, mode, name, configs[name], slices.Compact(ports), document.Volumes)
		if err != nil {
			return composeDocument{}, err
		}
		document.Services[name] = service
	}
	return document, nil
}

func configuredService(root, mode, name string, value any, targets []int, volumes map[string]struct{}) (composeService, error) {
	key := "services." + name
	if _, err := validateName(name, "service name"); err != nil {
		return composeService{}, err
	}
	config, err := configTable(value, key)
	if err != nil {
		return composeService{}, err
	}
	allowed := []string{"directory", "environment", "image", "command"}
	sourceMount := workspace
	if mode == "node" {
		allowed = []string{"directory", "environment", "script", "install"}
		sourceMount = nodeSource
	}
	if err := validateKeys(config, key, allowed...); err != nil {
		return composeService{}, err
	}
	directory, err := relativePath(root, valueOrDefault(config, "directory", "."), key+".directory")
	if err != nil {
		return composeService{}, err
	}
	directoryInfo, err := os.Stat(filepath.Join(root, filepath.FromSlash(directory)))
	if err != nil || !directoryInfo.IsDir() {
		return composeService{}, fmt.Errorf("service directory does not exist: %s", directory)
	}
	environment, err := configEnvironment(valueOrDefault(config, "environment", map[string]any{}), key+".environment")
	if err != nil {
		return composeService{}, err
	}
	service := newComposeService(directory, sourceMount, environment, targets)
	if mode == "node" {
		err := configureNodeService(root, directory, name, config, targets, volumes, &service)
		return service, err
	}
	image, err := nonemptyString(config["image"], key+".image")
	if err != nil {
		return composeService{}, err
	}
	service.Image = escapeCompose(image)
	service.Command, err = configCommand(config["command"], key+".command")
	return service, err
}

func configEnvironment(value any, key string) (map[string]string, error) {
	table, err := configTable(value, key)
	if err != nil {
		return nil, err
	}
	environment := make(map[string]string)
	for _, name := range slices.Sorted(maps.Keys(table)) {
		if !environmentName.MatchString(name) {
			return nil, fmt.Errorf("%s.%s must be a valid environment variable name", key, name)
		}
		setting, ok := table[name].(string)
		if !ok || strings.ContainsRune(setting, 0) {
			return nil, fmt.Errorf("%s.%s must be a string without null characters", key, name)
		}
		environment[name] = escapeCompose(setting)
	}
	return environment, nil
}

func configCommand(value any, key string) ([]string, error) {
	if _, ok := value.(string); ok {
		text, err := nonemptyString(value, key)
		if err != nil {
			return nil, err
		}
		return []string{"/bin/sh", "-c", escapeCompose(text)}, nil
	}
	arguments, ok := value.([]any)
	if !ok || len(arguments) == 0 {
		return nil, fmt.Errorf("%s must be shell text or a non-empty list of arguments", key)
	}
	if _, err := nonemptyString(arguments[0], key+"[0]"); err != nil {
		return nil, err
	}
	command := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		text, ok := argument.(string)
		if !ok || strings.ContainsRune(text, 0) {
			return nil, fmt.Errorf("%s must contain only strings without null characters", key)
		}
		command = append(command, escapeCompose(text))
	}
	return command, nil
}

func configureNodeService(root, directory, name string, config map[string]any, targets []int, volumes map[string]struct{}, service *composeService) error {
	key := "services." + name
	packageRoot := filepath.Join(root, filepath.FromSlash(directory))
	if !isRegularFile(filepath.Join(packageRoot, "package.json")) {
		return fmt.Errorf("%s.directory must contain a package.json: %s", key, directory)
	}
	if _, exists := service.Environment["HOST"]; !exists {
		service.Environment["HOST"] = "0.0.0.0"
	}
	if _, exists := service.Environment["PORT"]; !exists && len(targets) == 1 {
		service.Environment["PORT"] = strconv.Itoa(targets[0])
	}
	service.Image = defaultNodeImage
	script, err := nonemptyString(valueOrDefault(config, "script", "dev"), key+".script")
	if err != nil {
		return err
	}
	service.Labels = nodeLabels(manifestScriptWatches(packageRoot, script))
	installer, err := explicitNPMInstaller(packageRoot, valueOrDefault(config, "install", "auto"), key+".install")
	if err != nil {
		return err
	}
	startup := []string{"cp -a " + nodeSource + "/. " + workspace}
	if installer != "" {
		startup = append(startup, installer)
		volumeName := name + "-node-modules"
		volumes[volumeName] = struct{}{}
		service.Volumes = append(service.Volumes, composeVolume{
			Type: "volume", Source: volumeName,
			Target: escapeCompose(path.Join(workspace, directory, "node_modules")),
		})
	}
	startup = append(startup, `exec npm run -- "$$1"`)
	service.Command = []string{"/bin/sh", "-c", strings.Join(startup, " && "), "remote-dev", escapeCompose(script)}
	return nil
}

func explicitNPMInstaller(packageRoot string, value any, key string) (string, error) {
	install, ok := value.(string)
	if !ok || !slices.Contains([]string{"auto", "none", "ci", "install"}, install) {
		return "", fmt.Errorf("%s must be auto, none, ci or install", key)
	}
	if install == "none" {
		return "", nil
	}
	hasLock := isRegularFile(filepath.Join(packageRoot, "package-lock.json")) ||
		isRegularFile(filepath.Join(packageRoot, "npm-shrinkwrap.json"))
	if install == "ci" && !hasLock {
		return "", fmt.Errorf("%s requires a package-lock.json or npm-shrinkwrap.json when set to ci", key)
	}
	if install == "ci" || (install == "auto" && hasLock) {
		return "npm ci", nil
	}
	return "npm install --no-package-lock", nil
}

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	loopback       = "127.0.0.1"
	composeTimeout = "180"
	logLines       = "120"
	overlayFile    = ".remote-dev.generated.json"
)

var loopbackBinding = regexp.MustCompile(`^127\.0\.0\.1:([0-9]+)$`)
var tcpExposure = regexp.MustCompile(`^([0-9]+)(?:/tcp)?$`)

var infrastructure = map[string]bool{
	"database": true, "db": true, "postgres": true, "postgresql": true,
	"mysql": true, "mariadb": true, "redis": true, "valkey": true, "mongo": true,
	"mongodb": true, "rabbitmq": true, "memcached": true, "elasticsearch": true,
}

type forward struct {
	Name    string `json:"name"`
	Service string `json:"service"`
	Target  int    `json:"target"`
}

type composeSpec struct {
	document map[string]any
	services map[string]map[string]any
}

type hostCommand struct {
	context         context.Context
	progress        io.Writer
	ownProcessGroup bool
}

func (host hostCommand) docker(cwd string, arguments ...string) ([]byte, error) {
	return host.execute(cwd, nil, false, arguments...)
}

func (host hostCommand) dockerCommand(cwd string, arguments ...string) *exec.Cmd {
	command := exec.CommandContext(host.context, "docker", arguments...)
	command.Dir = cwd
	command.Env = composeEnvironment()
	if host.ownProcessGroup {
		isolateDockerProcess(command)
	}
	return command
}

func (host hostCommand) execute(cwd string, input io.Reader, live bool, arguments ...string) ([]byte, error) {
	command := host.dockerCommand(cwd, arguments...)
	command.Stdin = input
	var output, diagnostic bytes.Buffer
	command.Stdout, command.Stderr = &output, &diagnostic
	if live {
		command.Stdout, command.Stderr = host.progress, host.progress
	}
	if err := command.Run(); err != nil {
		if contextErr := host.context.Err(); host.ownProcessGroup && contextErr != nil {
			return nil, contextErr
		}
		detail := strings.TrimSpace(diagnostic.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("docker %s failed: %s", arguments[0], detail)
	}
	return output.Bytes(), nil
}

func composeEnvironment() []string {
	var variables []string
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "COMPOSE_") {
			variables = append(variables, variable)
		}
	}
	return variables
}

func composeCommand(project, source, filename string) ([]string, string, error) {
	composeFile, err := workspacePath(source, filename)
	if err != nil {
		return nil, "", err
	}
	info, err := os.Stat(composeFile)
	if err != nil || !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("Compose file missing from source snapshot: %s", filename)
	}
	return []string{"compose", "--project-name", project, "--file", composeFile}, filepath.Dir(composeFile), nil
}

func (host hostCommand) sourceCommand(project, source string, request hostRequest) ([]string, string, error) {
	command, cwd, err := composeCommand(project, source, request.ComposeFile)
	if err != nil {
		return nil, "", err
	}
	if request.ComposeOverride != "" {
		override, err := workspacePath(source, request.ComposeOverride)
		if err != nil {
			return nil, "", err
		}
		command = append(command, "--file", override)
	}
	if request.ComposeFile != overlayFile {
		overlay, err := workspacePath(source, overlayFile)
		if err != nil {
			return nil, "", err
		}
		if info, err := os.Stat(overlay); err == nil && info.Mode().IsRegular() {
			command = append(command, "--file", overlay)
		}
	}
	profiles := request.ComposeProfiles
	if request.AutoPorts && len(profiles) == 0 {
		output, err := host.docker(cwd, append(slices.Clone(command), "config", "--profiles")...)
		if err != nil {
			return nil, "", err
		}
		for _, available := range strings.Fields(string(output)) {
			if available == "dev" || available == "development" {
				profiles = append(profiles, available)
			}
		}
		slices.Sort(profiles)
	}
	for _, profile := range profiles {
		command = append(command, "--profile", profile)
	}
	return command, cwd, nil
}

func runtimeComposeCommand(workspace, project, source string) []string {
	return []string{"compose", "--project-name", project, "--project-directory", source, "--file", filepath.Join(workspace, "runtime-compose.json")}
}

func (host hostCommand) composeConfig(project, source string, request hostRequest, servicesToResolve ...string) (composeSpec, error) {
	command, cwd, err := host.sourceCommand(project, source, request)
	if err != nil {
		return composeSpec{}, err
	}
	arguments := append(command, "config", "--format", "json")
	output, err := host.docker(cwd, append(arguments, servicesToResolve...)...)
	if err != nil {
		return composeSpec{}, err
	}
	var document map[string]any
	if err := decodeJSON(bytes.NewReader(output), &document); err != nil {
		return composeSpec{}, fmt.Errorf("invalid Docker Compose configuration: %w", err)
	}
	if document == nil {
		return composeSpec{}, fmt.Errorf("invalid Docker Compose configuration")
	}
	services := map[string]map[string]any{}
	for name, raw := range object(document["services"]) {
		service, valid := raw.(map[string]any)
		if !valid {
			return composeSpec{}, fmt.Errorf("invalid Docker Compose service %s", name)
		}
		services[name] = service
	}
	return composeSpec{document: document, services: services}, nil
}

func object(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func list(value any) []any {
	result, _ := value.([]any)
	return result
}

func setting(value any) string {
	result, _ := value.(string)
	return result
}

func targetPort(value any) int {
	number, ok := value.(json.Number)
	if !ok {
		return 0
	}
	port, _ := strconv.Atoi(string(number))
	return port
}

func isTCP(binding map[string]any) bool {
	switch binding["protocol"] {
	case nil, "tcp":
		return true
	default:
		return false
	}
}

func imageName(service map[string]any) string {
	image := filepath.Base(setting(service["image"]))
	name, _, _ := strings.Cut(image, ":")
	name, _, _ = strings.Cut(name, "@")
	return name
}

func exportedPorts(service map[string]any) []int {
	targets := map[int]bool{}
	for _, value := range list(service["ports"]) {
		binding := object(value)
		port := targetPort(binding["target"])
		if isTCP(binding) && port >= 1 && port <= 65535 {
			targets[port] = true
		}
	}
	for _, value := range list(service["expose"]) {
		match := tcpExposure.FindStringSubmatch(fmt.Sprint(value))
		if len(match) == 2 {
			port, _ := strconv.Atoi(match[1])
			if port >= 1 && port <= 65535 {
				targets[port] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(targets))
}

func (config composeSpec) configureAutomaticPorts(declared []forward) ([]forward, error) {
	forwards := slices.Clone(declared)
	if len(forwards) == 0 {
		for _, name := range slices.Sorted(maps.Keys(config.services)) {
			service := config.services[name]
			if infrastructure[strings.ToLower(name)] || infrastructure[imageName(service)] {
				continue
			}
			ports := exportedPorts(service)
			for _, port := range ports {
				label := name
				if len(ports) != 1 {
					label = fmt.Sprintf("%s-%d", name, port)
				}
				forwards = append(forwards, forward{Name: label, Service: name, Target: port})
			}
		}
	}
	for _, service := range config.services {
		delete(service, "ports")
	}
	for _, binding := range forwards {
		service, exists := config.services[binding.Service]
		if !exists {
			return nil, fmt.Errorf("missing service for forward %s", binding.Name)
		}
		if countMatchingPorts(service, binding.Target) == 0 {
			port := map[string]any{"host_ip": loopback, "target": json.Number(strconv.Itoa(binding.Target)), "protocol": "tcp"}
			service["ports"] = append(list(service["ports"]), port)
		}
	}
	return forwards, config.validate(forwards)
}

func countMatchingPorts(service map[string]any, port int) int {
	count := 0
	for _, raw := range list(service["ports"]) {
		binding := object(raw)
		if targetPort(binding["target"]) == port && isTCP(binding) {
			count++
		}
	}
	return count
}

func (config composeSpec) validate(forwards []forward) error {
	for _, name := range slices.Sorted(maps.Keys(config.services)) {
		service := config.services[name]
		if setting(service["network_mode"]) == "host" || setting(service["container_name"]) != "" {
			return fmt.Errorf("service %s must use project networking and a Compose-generated container name", name)
		}
		for _, raw := range list(service["ports"]) {
			if object(raw)["host_ip"] != loopback {
				return fmt.Errorf("bind every published port for %s to %s in Compose", name, loopback)
			}
		}
	}
	for _, binding := range forwards {
		if countMatchingPorts(config.services[binding.Service], binding.Target) != 1 {
			return fmt.Errorf("publish exactly one TCP port %d on service %s", binding.Target, binding.Service)
		}
	}
	return nil
}

func (config composeSpec) usesBuild() bool {
	for _, service := range config.services {
		if _, exists := service["build"]; exists {
			return true
		}
	}
	return false
}

func escapeComposeValue(value any) any {
	switch value := value.(type) {
	case string:
		return strings.ReplaceAll(value, "$", "$$")
	case []any:
		result := make([]any, len(value))
		for index, item := range value {
			result[index] = escapeComposeValue(item)
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, item := range value {
			result[key] = escapeComposeValue(item)
		}
		return result
	default:
		return value
	}
}

func (host hostCommand) inspectPorts(command []string, cwd string, forwards []forward) (map[string]int, error) {
	ports := map[string]int{}
	for _, binding := range forwards {
		output, err := host.docker(cwd, append(slices.Clone(command), "port", binding.Service, strconv.Itoa(binding.Target))...)
		if err != nil {
			return nil, err
		}
		match := loopbackBinding.FindStringSubmatch(strings.TrimSpace(string(output)))
		if len(match) == 2 {
			port, _ := strconv.Atoi(match[1])
			if port >= 1 && port <= 65535 {
				ports[binding.Name] = port
				continue
			}
		}
		return nil, fmt.Errorf("no loopback binding for %s", binding.Name)
	}
	return ports, nil
}

func (host hostCommand) composeUp(command []string, cwd string, rebuild, recreate bool, services []string) error {
	args := append(slices.Clone(command), "up", "--detach", "--wait", "--wait-timeout", composeTimeout)
	if len(services) == 0 {
		args = append(args, "--remove-orphans")
	} else {
		args = append(args, "--no-deps")
	}
	if rebuild {
		args = append(args, "--build")
	}
	if recreate {
		args = append(args, "--force-recreate")
	}
	_, err := host.execute(cwd, nil, true, append(args, services...)...)
	return err
}

func projectFilter(project string) string {
	return "label=com.docker.compose.project=" + project
}

func (host hostCommand) runningServices(project string) (map[string][]string, error) {
	rows, err := host.docker("", "ps", "--filter", projectFilter(project), "--format", "{{.ID}}\t{{.Label \"com.docker.compose.service\"}}")
	if err != nil {
		return nil, err
	}
	services := map[string][]string{}
	for _, row := range strings.Split(strings.TrimSpace(string(rows)), "\n") {
		container, service, valid := strings.Cut(row, "\t")
		if valid && container != "" && service != "" {
			services[service] = append(services[service], container)
		}
	}
	return services, nil
}

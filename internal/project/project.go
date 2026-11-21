package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	ConfigFile           = "remote-dev.toml"
	GeneratedComposeFile = ".remote-dev.generated.json"
	defaultComposeFile   = "compose.yaml"
	workspace            = "/workspace"
	nodeSource           = "/remote-source"
	defaultNodeImage     = "node:24-bookworm-slim"
	maxTCPPort           = 65535
)

var (
	projectName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	sshName     = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
)

type Forward struct {
	Name    string `json:"name"`
	Service string `json:"service"`
	Target  int    `json:"target"`
	Local   *int   `json:"local"`
}

type Project struct {
	Mode         string
	Host         string
	ComposeFile  string
	Override     string
	Profiles     []string
	Forwards     []Forward
	AutoPorts    bool
	Generated    []byte
	DetectedFrom string
}

func Load(root string) (Project, error) {
	return LoadWithHost(root, "")
}

func LoadWithHost(root, host string) (Project, error) {
	if host != "" && !validHostOverride(host) {
		return Project{}, errors.New("use a literal SSH host alias or user@hostname")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return Project{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Project{}, err
	}
	filename := filepath.Join(root, ConfigFile)
	_, err = os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return detectProject(root, host)
	}
	if err != nil {
		return Project{}, fmt.Errorf("read %s: %w", filename, err)
	}
	info, err := os.Stat(filename)
	if err != nil {
		return Project{}, fmt.Errorf("read %s: %w", filename, err)
	}
	if !info.Mode().IsRegular() {
		return Project{}, fmt.Errorf("expected a regular file for %s", filename)
	}
	return loadConfiguration(root, filename, host)
}

func validHostOverride(host string) bool {
	user, hostname, hasUser := strings.Cut(host, "@")
	if hasUser && !sshName.MatchString(user) {
		return false
	}
	if !hasUser {
		hostname = user
	}
	if sshName.MatchString(hostname) {
		return true
	}
	if strings.HasPrefix(hostname, "[") && strings.HasSuffix(hostname, "]") {
		hostname = hostname[1 : len(hostname)-1]
	}
	address, err := netip.ParseAddr(hostname)
	return err == nil && address.Is6() && address.Zone() == ""
}

func projectFile(root, relative string) (bool, error) {
	filename := filepath.Join(root, relative)
	info, err := os.Stat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", relative, err)
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	resolved, err := filepath.EvalSymlinks(filename)
	if err != nil {
		return false, fmt.Errorf("resolve %s: %w", relative, err)
	}
	if resolved != filename {
		return false, fmt.Errorf("autodetection requires a regular file in the project: %s", relative)
	}
	return true, nil
}

type composeDocument struct {
	Services map[string]composeService `json:"services"`
	Volumes  map[string]struct{}       `json:"volumes,omitempty"`
}

type composeService struct {
	Init        bool              `json:"init"`
	Image       string            `json:"image"`
	WorkingDir  string            `json:"working_dir"`
	Volumes     []composeVolume   `json:"volumes"`
	Ports       []composePort     `json:"ports,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Command     []string          `json:"command"`
}

type composeVolume struct {
	Type     string `json:"type"`
	Source   string `json:"source,omitempty"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

type composePort struct {
	Target   int    `json:"target"`
	Protocol string `json:"protocol"`
	HostIP   string `json:"host_ip"`
}

func newComposeService(directory, sourceMount string, environment map[string]string, targets []int) composeService {
	service := composeService{
		Init:        true,
		WorkingDir:  escapeCompose(path.Join(workspace, directory)),
		Volumes:     []composeVolume{{Type: "bind", Source: ".", Target: sourceMount, ReadOnly: true}},
		Environment: environment,
	}
	for _, target := range targets {
		service.Ports = append(service.Ports, composePort{Target: target, Protocol: "tcp", HostIP: "127.0.0.1"})
	}
	return service
}

func withGenerated(project Project, document composeDocument) (Project, error) {
	generated, err := json.Marshal(document)
	if err != nil {
		return Project{}, fmt.Errorf("generate Compose document: %w", err)
	}
	project.Generated = generated
	if project.ComposeFile == "" {
		project.ComposeFile = GeneratedComposeFile
	}
	return project, nil
}

func escapeCompose(value string) string {
	return strings.ReplaceAll(value, "$", "$$")
}

func nodeLabels(watches bool) map[string]string {
	reload := "restart"
	if watches {
		reload = "watch"
	}
	return map[string]string{"io.remote-dev.live-source": workspace, "io.remote-dev.reload": reload}
}

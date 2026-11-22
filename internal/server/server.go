package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

const version = "remote-dev-helper 1"
const lockPollInterval = 100 * time.Millisecond

var workspaceKey = regexp.MustCompile(`^[0-9a-f]{24}$`)
var forwardName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

type hostRequest struct {
	Digest          string    `json:"digest"`
	ComposeFile     string    `json:"compose_file"`
	ComposeOverride string    `json:"compose_override"`
	ComposeProfiles []string  `json:"compose_profiles"`
	Forwards        []forward `json:"forwards"`
	AutoPorts       bool      `json:"auto_ports"`
	Delta           bool      `json:"delta"`
	TestService     string    `json:"test_service,omitempty"`
	TestBytes       int64     `json:"test_bytes,omitempty"`
}

type deployment struct {
	Digest        string    `json:"digest"`
	ComposeFile   string    `json:"compose_file"`
	RuntimeSource bool      `json:"runtime_source"`
	Forwards      []forward `json:"forwards"`
}

type sourceJournal struct {
	Previous *string `json:"previous"`
	Desired  string  `json:"desired"`
}

type activationReply struct {
	Running bool           `json:"running"`
	Ports   map[string]int `json:"ports"`
}

type fullSnapshotReply struct {
	NeedFull bool `json:"need_full"`
}

func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 1 && args[0] == "--version" {
		_, err := fmt.Fprintln(stdout, version)
		return err
	}
	host := hostCommand{context: ctx, progress: stderr}
	if len(args) > 0 && args[0] == "doctor" {
		if len(args) != 1 && len(args) != 3 {
			return errors.New("usage: remote-dev-helper doctor")
		}
		result, err := host.doctor()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, result)
		return err
	}
	if len(args) != 3 {
		return errors.New("usage: remote-dev-helper ACTION WORKSPACE REQUEST_JSON")
	}
	action, key := args[0], args[1]
	if !workspaceKey.MatchString(key) {
		return errors.New("invalid workspace identity")
	}
	var request hostRequest
	if err := decodeJSON(strings.NewReader(args[2]), &request); err != nil {
		return fmt.Errorf("invalid host request: %w", err)
	}
	if action == "test" {
		result, err := host.runTest(key, request, stdin)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(result)
	}
	workspace, err := workspaceDirectory(key)
	if err != nil {
		return err
	}
	project := "rd-" + key
	switch action {
	case "up", "sync":
		if err := validateRequest(request); err != nil {
			return err
		}
		if stdin == nil {
			stdin = strings.NewReader("")
		}
		return withWorkspaceLock(ctx, workspace, func() error {
			result, err := host.activate(workspace, project, action, request, stdin)
			if err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(result)
		})
	case "stop", "status", "logs":
		available, err := fileExists(workspace)
		if err != nil {
			return err
		}
		if !available {
			_, err := fmt.Fprintln(stdout, "Workspace has not run yet. Use remote-dev up.")
			return err
		}
		return withWorkspaceLock(ctx, workspace, func() error {
			result, err := host.manage(workspace, project, action)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(stdout, result)
			return err
		})
	default:
		return fmt.Errorf("unknown host operation: %s", action)
	}
}

func validateRequest(request hostRequest) error {
	if !snapshotDigest.MatchString(request.Digest) {
		return errors.New("invalid snapshot digest")
	}
	if request.ComposeFile == "" {
		return errors.New("missing Compose file")
	}
	seen := map[string]bool{}
	for _, binding := range request.Forwards {
		if !forwardName.MatchString(binding.Name) || !forwardName.MatchString(binding.Service) {
			return fmt.Errorf("invalid forward %q", binding.Name)
		}
		if binding.Target < 1 || binding.Target > 65535 {
			return fmt.Errorf("invalid target port for forward %q", binding.Name)
		}
		if seen[binding.Name] {
			return fmt.Errorf("duplicate forward %q", binding.Name)
		}
		seen[binding.Name] = true
	}
	return nil
}

func workspaceDirectory(key string) (string, error) {
	root, err := dataDirectory()
	if err != nil {
		return "", err
	}
	return workspacePath(root, "remote-dev/standalone-v1/"+key)
}

func dataDirectory() (string, error) {
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		data = filepath.Join(home, ".local", "share")
	}
	return filepath.Abs(data)
}

func withWorkspaceLock(ctx context.Context, workspace string, operation func() error) error {
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return err
	}
	filename := filepath.Join(workspace, "operation.lock")
	lock, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	stat, err := lock.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() {
		return errors.New("invalid workspace lock file")
	}
	ticker := time.NewTicker(lockPollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return operation()
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (host hostCommand) doctor() (string, error) {
	engine, err := host.docker("", "info", "--format", "{{.OSType}}")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(engine)) != "linux" {
		return "", errors.New("expected a Linux Docker engine")
	}
	compose, err := host.docker("", "compose", "version")
	if err != nil {
		return "", err
	}
	return "Linux · " + strings.TrimSpace(string(compose)), nil
}

func (host hostCommand) manage(workspace, project, action string) (string, error) {
	switch action {
	case "status":
		output, err := host.docker("", "ps", "--all", "--filter", projectFilter(project), "--format", "table {{.Label \"com.docker.compose.service\"}}\t{{.Status}}")
		return strings.TrimSpace(string(output)), err
	case "logs":
		return host.logs(workspace, project)
	default:
		return host.stop(project)
	}
}

func (host hostCommand) stop(project string) (string, error) {
	output, err := host.docker("", "ps", "--quiet", "--filter", projectFilter(project))
	if err != nil {
		return "", err
	}
	containers := strings.Fields(string(output))
	if len(containers) > 0 {
		if _, err := host.execute("", nil, true, append([]string{"stop"}, containers...)...); err != nil {
			return "", err
		}
	}
	return "Workspace stopped.", nil
}

func (host hostCommand) logs(workspace, project string) (string, error) {
	var saved deployment
	if err := readJSON(filepath.Join(workspace, "deployment.json"), &saved); err != nil {
		return "", err
	}
	runtimeConfig, err := fileExists(filepath.Join(workspace, "runtime-compose.json"))
	if err != nil {
		return "", err
	}
	cwd := runtimeSource(workspace)
	command := runtimeComposeCommand(workspace, project, cwd)
	if !saved.RuntimeSource && !runtimeConfig {
		source, err := sourcePath(workspace, saved.Digest)
		if err != nil {
			return "", err
		}
		command, cwd, err = composeCommand(project, source, saved.ComposeFile)
		if err != nil {
			return "", err
		}
	}
	output, err := host.docker(cwd, append(command, "logs", "--no-color", "--tail", logLines)...)
	return strings.TrimSpace(string(output)), err
}

func readWorkspaceJSON(workspace, name string, output any) error {
	filename, err := workspacePath(workspace, name)
	if err != nil {
		return err
	}
	err = readJSON(filename, output)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (host hostCommand) activate(workspace, project, action string, request hostRequest, input io.Reader) (any, error) {
	var previous deployment
	if err := readWorkspaceJSON(workspace, "deployment.json", &previous); err != nil {
		return nil, err
	}
	previousSource, err := optionalSnapshot(workspace, previous.Digest)
	if err != nil {
		return nil, err
	}
	desired, err := acceptSnapshot(workspace, request, previous.Digest, input)
	if err != nil {
		return nil, err
	}
	if desired == "" {
		return fullSnapshotReply{NeedFull: true}, nil
	}
	forwards, err := host.previewRuntime(project, desired, request)
	if err != nil {
		return nil, err
	}
	var interrupted sourceJournal
	if err := readWorkspaceJSON(workspace, "source-update.json", &interrupted); err != nil {
		return nil, err
	}
	baseline := previousSource
	if !previous.RuntimeSource {
		baseline = ""
	}
	recovered, err := recoverRuntime(workspace, interrupted)
	if err != nil {
		return nil, err
	}
	if recovered != "" {
		baseline = recovered
	}
	if err := checkRuntimeSource(workspace, baseline); err != nil {
		return nil, err
	}
	journal := sourceJournal{Desired: request.Digest}
	if baseline != "" {
		base := filepath.Base(filepath.Dir(baseline))
		journal.Previous = &base
	}
	journalFile := filepath.Join(workspace, "source-update.json")
	if err := writeJSON(journalFile, journal); err != nil {
		return nil, err
	}
	changed, deleted, err := applyRuntimeSource(workspace, baseline, desired)
	if err != nil {
		return nil, err
	}
	ports, running, err := host.launchRuntime(workspace, project, action, request, forwards, changed, deleted, recovered != "")
	if err != nil {
		return nil, err
	}
	if forwards == nil {
		forwards = []forward{}
	}
	if err := writeJSON(filepath.Join(workspace, "deployment.json"), deployment{
		Digest: request.Digest, ComposeFile: request.ComposeFile, RuntimeSource: true, Forwards: forwards,
	}); err != nil {
		return nil, err
	}
	if err := os.Remove(journalFile); err != nil {
		return nil, err
	}
	if err := pruneOldSnapshots(previousSource, desired, recovered, previous.RuntimeSource); err != nil {
		return nil, err
	}
	return activationReply{Running: running, Ports: ports}, nil
}

func optionalSnapshot(workspace, digest string) (string, error) {
	if digest == "" {
		return "", nil
	}
	return verifySnapshot(workspace, digest)
}

func acceptSnapshot(workspace string, request hostRequest, deployed string, input io.Reader) (string, error) {
	if request.Delta {
		accepted, err := installDelta(workspace, request.Digest, deployed, input)
		if err != nil || !accepted {
			return "", err
		}
	} else if err := installSnapshot(workspace, request.Digest, input); err != nil {
		return "", err
	}
	return verifySnapshot(workspace, request.Digest)
}

func (host hostCommand) previewRuntime(project, desired string, request hostRequest) ([]forward, error) {
	config, err := host.composeConfig(project, desired, request)
	if err != nil {
		return nil, err
	}
	if request.AutoPorts {
		return config.configureAutomaticPorts(request.Forwards)
	}
	return request.Forwards, config.validate(request.Forwards)
}

func recoverRuntime(workspace string, interrupted sourceJournal) (string, error) {
	if interrupted.Desired == "" {
		return "", nil
	}
	var previous string
	if interrupted.Previous != nil {
		var err error
		previous, err = verifySnapshot(workspace, *interrupted.Previous)
		if err != nil {
			return "", err
		}
	}
	desired, err := verifySnapshot(workspace, interrupted.Desired)
	if err != nil {
		return "", err
	}
	if _, _, err := applyRuntimeSource(workspace, previous, desired); err != nil {
		return "", err
	}
	return desired, nil
}

func checkRuntimeSource(workspace, baseline string) error {
	source, err := workspacePath(workspace, "runtime-source")
	if err != nil {
		return err
	}
	expected, err := baselineInventory(baseline)
	if err != nil {
		return err
	}
	actual, err := inventory(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !maps.Equal(expected, actual) {
		return errors.New("remote source changed. Preserve that edit before syncing")
	}
	return nil
}

func pruneOldSnapshots(previous, desired, recovered string, oldRuntime bool) error {
	if oldRuntime && previous != "" && previous != desired {
		if err := os.RemoveAll(filepath.Dir(previous)); err != nil {
			return err
		}
	}
	if recovered != "" && recovered != previous && recovered != desired {
		return os.RemoveAll(filepath.Dir(recovered))
	}
	return nil
}

func (host hostCommand) launchRuntime(workspace, project, action string, request hostRequest, forwards []forward, changed, deleted []string, recovered bool) (map[string]int, bool, error) {
	source := runtimeSource(workspace)
	config, err := host.composeConfig(project, source, request)
	if err != nil {
		return nil, false, err
	}
	if request.AutoPorts {
		_, err = config.configureAutomaticPorts(forwards)
	} else {
		err = config.validate(forwards)
	}
	if err != nil {
		return nil, false, err
	}
	configChanged, err := saveRuntimeConfig(workspace, config)
	if err != nil {
		return nil, false, err
	}
	command := runtimeComposeCommand(workspace, project, source)
	containers, err := host.runningServices(project)
	if err != nil {
		return nil, false, err
	}
	running := action == "up" || len(containers) > 0
	if !running {
		return map[string]int{}, false, nil
	}
	if action == "up" || configChanged || recovered {
		if err := host.composeUp(command, source, config.usesBuild(), recovered, nil); err != nil {
			return nil, false, err
		}
		if !configChanged && !recovered {
			containers, err = host.runningServices(project)
			if err != nil {
				return nil, false, err
			}
		}
	}
	if !configChanged && !recovered {
		if err := host.refreshServices(command, source, config, containers, changed, deleted); err != nil {
			return nil, false, err
		}
	}
	ports, err := host.inspectPorts(command, source, forwards)
	return ports, true, err
}

func saveRuntimeConfig(workspace string, config composeSpec) (bool, error) {
	for _, service := range config.services {
		delete(service, "profiles")
	}
	resolved := escapeComposeValue(config.document)
	var previous map[string]any
	if err := readWorkspaceJSON(workspace, "runtime-compose.json", &previous); err != nil {
		return false, err
	}
	encoded, err := json.Marshal(resolved)
	if err != nil {
		return false, err
	}
	oldEncoded, err := json.Marshal(previous)
	if err != nil {
		return false, err
	}
	if slices.Equal(encoded, oldEncoded) {
		return false, nil
	}
	return true, writeJSON(filepath.Join(workspace, "runtime-compose.json"), resolved)
}

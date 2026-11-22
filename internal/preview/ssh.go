package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Ross-cripto/remote-dev/internal/project"
)

const (
	loopback      = "127.0.0.1"
	remoteTimeout = 4 * time.Minute
)

var sshName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
var versionNumber = regexp.MustCompile(`[0-9]+\.[0-9]+(?:\.[0-9]+)?`)

type Connection struct {
	Target       string `json:"target"`
	Identity     string `json:"identity,omitempty"`
	Architecture string `json:"architecture,omitempty"`
}

func validTarget(target string) bool {
	user, host, hasUser := strings.Cut(target, "@")
	if hasUser && !sshName.MatchString(user) {
		return false
	}
	if !hasUser {
		host = user
	}
	if sshName.MatchString(host) {
		return true
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return strings.Contains(host, ":") && net.ParseIP(host) != nil
}

func newConnection(target, identity string) (Connection, error) {
	if !validTarget(target) {
		return Connection{}, errors.New("use an OpenSSH alias or user@hostname")
	}
	target = strings.NewReplacer("[", "", "]", "").Replace(target)
	if identity != "" {
		if strings.Contains(identity, "%") || strings.Contains(identity, "${") || strings.ContainsAny(identity, "\r\n\t") {
			return Connection{}, errors.New("use a local SSH identity filename without expansion tokens")
		}
		var err error
		identity, err = filepath.Abs(identity)
		if err != nil {
			return Connection{}, err
		}
		info, err := os.Stat(identity)
		if err != nil {
			return Connection{}, err
		}
		if !info.Mode().IsRegular() {
			return Connection{}, errors.New("SSH identity must be a file")
		}
	}
	return Connection{Target: target, Identity: identity}, nil
}

func (connection Connection) options(interactive bool) []string {
	options := []string{"-o", "ConnectTimeout=8", "-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=3"}
	if interactive {
		options = append(options, "-o", "StrictHostKeyChecking=ask")
	} else {
		options = append(options, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes")
	}
	if connection.Identity != "" {
		options = append(options, "-o", "IdentitiesOnly=yes", "-i", connection.Identity)
	}
	return options
}

func shellCommand(arguments ...string) string {
	words := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		words = append(words, "'"+strings.ReplaceAll(argument, "'", "'\"'\"'")+"'")
	}
	return strings.Join(words, " ")
}

func commandOutput(ctx context.Context, executable string, input io.Reader, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Stdin = input
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	output, err := command.Output()
	if err != nil {
		detail := strings.TrimSpace(diagnostic.String())
		if detail != "" {
			return nil, fmt.Errorf("%s: %s: %w", executable, detail, err)
		}
		return nil, fmt.Errorf("%s: %w", executable, err)
	}
	return output, nil
}

func (connection Connection) commandOptions(command string, interactive bool) []string {
	options := append(connection.options(interactive), "-T", "-o", "ClearAllForwardings=yes", "-o", "ControlPath=none")
	return append(options, "--", connection.Target, command)
}

func (connection Connection) command(ctx context.Context, input io.Reader, command string, interactive bool) ([]byte, error) {
	return commandOutput(ctx, "ssh", input, connection.commandOptions(command, interactive)...)
}

func meetsVersion(value string, requiredMajor, requiredMinor int) bool {
	number := versionNumber.FindString(value)
	parts := strings.Split(number, ".")
	if len(parts) < 2 {
		return false
	}
	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])
	return major > requiredMajor || major == requiredMajor && minor >= requiredMinor
}

func (connection Connection) doctor(ctx context.Context, interactive bool) (string, error) {
	facts, err := connection.probe(ctx, interactive)
	if err != nil {
		return "", err
	}
	if err := facts.ready(); err != nil {
		return "", err
	}
	_, detail, err := connection.prepared(ctx, facts)
	return detail, err
}

type hostRequest struct {
	Digest      string            `json:"digest"`
	Compose     string            `json:"compose_file"`
	Override    string            `json:"compose_override,omitempty"`
	Forwards    []project.Forward `json:"forwards"`
	Profiles    []string          `json:"compose_profiles,omitempty"`
	AutoPorts   bool              `json:"auto_ports"`
	Delta       bool              `json:"delta,omitempty"`
	TestService string            `json:"test_service,omitempty"`
	TestBytes   int64             `json:"test_bytes,omitempty"`
}

type hostReply struct {
	Running  bool           `json:"running"`
	Ports    map[string]int `json:"ports"`
	NeedFull bool           `json:"need_full"`
}

func (connection Connection) host(ctx context.Context, action, key string, request hostRequest, input io.Reader) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, remoteTimeout)
	defer cancel()
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	return connection.invokeHelper(ctx, input, action, key, string(payload))
}

func control(ctx context.Context, socket, action string) error {
	_, err := commandOutput(ctx, "ssh", nil, "-F", "/dev/null", "-S", socket, "-O", action, "localhost")
	return err
}

func clearSocket(socket string) error {
	info, err := os.Lstat(socket)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return fmt.Errorf("refusing to remove a socket path not owned by this client: %s", socket)
	}
	return os.Remove(socket)
}

func listenLoopback(ctx context.Context, port int) (net.Listener, error) {
	config := net.ListenConfig{
		Control: func(network, address string, connection syscall.RawConn) error {
			var optionError error
			err := connection.Control(func(descriptor uintptr) {
				optionError = syscall.SetsockoptInt(int(descriptor), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			})
			return errors.Join(err, optionError)
		},
	}
	return config.Listen(ctx, "tcp4", net.JoinHostPort(loopback, strconv.Itoa(port)))
}

func allocatePorts(ctx context.Context, desired, previous, fixed map[string]int) (map[string]int, error) {
	ports := make(map[string]int, len(desired))
	listeners := make([]net.Listener, 0, len(desired))
	defer func() {
		for _, listener := range listeners {
			listener.Close()
		}
	}()
	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		port := fixed[name]
		if port == 0 {
			port = previous[name]
		}
		listener, err := listenLoopback(ctx, port)
		if err != nil && fixed[name] != 0 {
			return nil, fmt.Errorf("local port %d is unavailable for %s: %w", port, name, err)
		}
		if err != nil && port != 0 {
			listener, err = listenLoopback(ctx, 0)
		}
		if err != nil {
			return nil, err
		}
		listeners = append(listeners, listener)
		ports[name] = listener.Addr().(*net.TCPAddr).Port
	}
	return ports, nil
}

func (connection Connection) openTunnel(ctx context.Context, socket string, remotePorts, preferred, fixed map[string]int) (map[string]int, error) {
	localPorts, err := allocatePorts(ctx, remotePorts, preferred, fixed)
	if err != nil {
		return nil, err
	}
	options := append(connection.options(false), "-M", "-S", socket, "-fNT",
		"-o", "ExitOnForwardFailure=yes", "-o", "ControlPersist=no", "-o", "ClearAllForwardings=yes",
		"-o", "GatewayPorts=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no",
	)
	if _, err := commandOutput(ctx, "ssh", nil, append(options, "--", connection.Target)...); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(remotePorts))
	for name := range remotePorts {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		remotePort := remotePorts[name]
		forward := fmt.Sprintf("%s:%d:%s:%d", loopback, localPorts[name], loopback, remotePort)
		if _, err := commandOutput(ctx, "ssh", nil, "-F", "/dev/null", "-S", socket, "-O", "forward", "-L", forward, "localhost"); err != nil {
			return nil, errors.Join(err, control(ctx, socket, "exit"))
		}
	}
	return localPorts, nil
}

package preview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/Ross-cripto/remote-dev/internal/project"
)

func (client Client) closeTunnel(ctx context.Context, workspace Workspace) error {
	socket := client.socket(workspace)
	if control(ctx, socket, "check") == nil {
		if err := control(ctx, socket, "exit"); err != nil {
			return err
		}
	}
	return clearSocket(socket)
}

func (client Client) ensureTunnel(ctx context.Context, workspace *Workspace) error {
	if len(workspace.RemotePorts) == 0 {
		return nil
	}
	socket := client.socket(*workspace)
	if control(ctx, socket, "check") == nil {
		return nil
	}
	if err := clearSocket(socket); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return err
	}
	ports, err := workspace.Connection.openTunnel(ctx, socket, workspace.RemotePorts, workspace.LocalPorts, workspace.FixedPorts)
	if err != nil {
		return err
	}
	workspace.LocalPorts = ports
	return client.saveWorkspace(*workspace)
}

func sendSnapshot(ctx context.Context, workspace Workspace, action string, snapshot *Snapshot, request hostRequest, delta bool) (hostReply, error) {
	defer snapshot.Close()
	request.Digest, request.Delta = snapshot.Digest, delta
	output, err := workspace.Connection.host(ctx, action, workspace.Key, request, snapshot.File)
	if err != nil {
		return hostReply{}, err
	}
	var reply hostReply
	if err := json.Unmarshal(output, &reply); err != nil {
		return reply, fmt.Errorf("decode response from Linux helper: %w", err)
	}
	for name, port := range reply.Ports {
		if port < 1 || port > 65535 {
			return hostReply{}, fmt.Errorf("invalid remote port for %s: %d", name, port)
		}
	}
	return reply, nil
}

func runtimeRequest(detected project.Project) hostRequest {
	return hostRequest{
		Compose: detected.ComposeFile, Override: detected.Override,
		Forwards: append([]project.Forward{}, detected.Forwards...),
		Profiles: detected.Profiles, AutoPorts: detected.AutoPorts,
	}
}

func fixedPorts(forwards []project.Forward) map[string]int {
	result := map[string]int{}
	for _, forward := range forwards {
		if forward.Local != nil {
			result[forward.Name] = *forward.Local
		}
	}
	return result
}

func hasFixedPortDiff(fixed, current map[string]int) bool {
	for name, port := range fixed {
		if port != current[name] {
			return true
		}
	}
	return false
}

func (client Client) deploy(ctx context.Context, workspace *Workspace, action string, full bool) (bool, error) {
	detected, err := project.LoadWithHost(workspace.Root, workspace.HostOverride)
	if err != nil {
		return false, err
	}
	if detected.Host != "" {
		connection, err := client.resolveConnection(detected.Host)
		if err != nil {
			return false, err
		}
		if connection.Target != workspace.Connection.Target {
			return false, errors.New("the project names a different SSH server than its registered workspace")
		}
	}
	request := runtimeRequest(detected)
	configuration, err := json.Marshal(request)
	if err != nil {
		return false, err
	}
	hash := sha256.Sum256(configuration)
	configDigest := hex.EncodeToString(hash[:])
	full = full || workspace.ConfigDigest != configDigest

	var base string
	var records map[string]FileStamp
	if !full {
		base, records = workspace.Digest, workspace.Records
	}
	snapshot, err := makeProjectSnapshot(ctx, workspace.Root, records, base, detected.Generated)
	if err != nil {
		return false, err
	}
	if snapshot == nil {
		return false, nil
	}
	reply, err := sendSnapshot(ctx, *workspace, action, snapshot, request, base != "")
	if err != nil {
		return false, err
	}
	if reply.NeedFull {
		if full {
			return false, errors.New("server requested a full snapshot after a full transfer")
		}
		return client.deploy(ctx, workspace, action, true)
	}

	fixed := fixedPorts(detected.Forwards)
	if !maps.Equal(workspace.RemotePorts, reply.Ports) || hasFixedPortDiff(fixed, workspace.LocalPorts) {
		if err := client.closeTunnel(ctx, *workspace); err != nil {
			return false, err
		}
	}
	workspace.Digest, workspace.Records = snapshot.Digest, snapshot.Records
	workspace.RemotePorts = reply.Ports
	workspace.Mode = detected.Mode
	workspace.Compose = Compose{File: detected.ComposeFile, Override: detected.Override}
	workspace.ConfigDigest = configDigest
	workspace.FixedPorts = fixed
	workspace.WatchError = ""
	stats := finishTransfer(snapshot, base == "")
	workspace.LastSync = &stats
	if err := client.saveWorkspace(*workspace); err != nil {
		return false, err
	}
	if reply.Running {
		return true, client.ensureTunnel(ctx, workspace)
	}
	return true, nil
}

func printLinks(writer io.Writer, workspace Workspace) {
	names := make([]string, 0, len(workspace.RemotePorts))
	for name := range workspace.RemotePorts {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if port := workspace.LocalPorts[name]; port != 0 {
			fmt.Fprintf(writer, "%s  http://%s:%d\n", name, loopback, port)
		}
	}
}

func (client Client) up(ctx context.Context, writer io.Writer, root, host string, watching bool) error {
	detected, err := project.LoadWithHost(root, host)
	if err != nil {
		return err
	}
	return client.underWorkspaceLock(root, func() error {
		workspace, err := client.readWorkspace(root)
		if errors.Is(err, os.ErrNotExist) {
			if _, legacyErr := findLegacyWorkspace(client.legacyState(), root, detected.Host); legacyErr == nil {
				return errors.New("a Python session already exists for this worktree. Run remote-dev migrate to keep it")
			} else if !errors.Is(legacyErr, os.ErrNotExist) {
				return legacyErr
			}
			connection, err := client.resolveConnection(detected.Host)
			if err != nil {
				return err
			}
			key, err := randomID()
			if err != nil {
				return err
			}
			workspace = Workspace{Root: root, Key: key, Connection: connection}
		} else if err != nil {
			return err
		}
		if workspace.Connection.Architecture == "" {
			workspace.Connection, err = client.readyConnection(ctx, workspace.Connection)
			if err != nil {
				return err
			}
		}
		if host != "" {
			workspace.HostOverride = host
		}
		if err := client.saveWorkspace(workspace); err != nil {
			return err
		}
		if _, err := client.deploy(ctx, &workspace, "up", true); err != nil {
			return err
		}
		if err := client.setWatcher(&workspace, watching); err != nil {
			return err
		}

		fmt.Fprintf(writer, "Workspace running.\n%s · %s\n", workspace.Mode, workspace.Compose)
		printLinks(writer, workspace)
		fmt.Fprintf(writer, "Sent %s.\n", workspace.LastSync)
		if len(workspace.RemotePorts) == 0 {
			fmt.Fprintln(writer, "No app ports detected. Publish a TCP port in Compose.")
		}
		if watching {
			fmt.Fprintln(writer, "Changes sync on save.")
		}
		return nil
	})
}

func (client Client) existing(root string, operation func(*Workspace) error) error {
	return client.underWorkspaceLock(root, func() error {
		workspace, err := client.readWorkspace(root)
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("this worktree has no Go workspace. Use remote-dev up or migrate")
		}
		if err != nil {
			return err
		}
		return operation(&workspace)
	})
}

func (client Client) workspaceAction(ctx context.Context, writer io.Writer, root, action string) error {
	return client.existing(root, func(workspace *Workspace) error {
		switch action {
		case "sync":
			changed, err := client.deploy(ctx, workspace, "sync", false)
			if err != nil {
				return err
			}
			if changed {
				fmt.Fprintf(writer, "Sent %s.\n", workspace.LastSync)
			} else {
				fmt.Fprintln(writer, "Source has no changes.")
			}
			printLinks(writer, *workspace)
			return nil
		case "stop":
			if err := client.setWatcher(workspace, false); err != nil {
				return err
			}
			if err := client.closeTunnel(ctx, *workspace); err != nil {
				return err
			}
		case "open":
			return client.open(ctx, writer, *workspace)
		}
		output, err := workspace.Connection.host(ctx, action, workspace.Key, hostRequest{}, nil)
		if err != nil {
			return err
		}
		if action == "stop" {
			workspace.RemotePorts = nil
			if err := client.saveWorkspace(*workspace); err != nil {
				return err
			}
		}
		fmt.Fprintln(writer, string(output))
		if action == "status" {
			if control(ctx, client.socket(*workspace), "check") == nil {
				printLinks(writer, *workspace)
			}
			if workspace.LastSync != nil {
				fmt.Fprintf(writer, "Last sync: %s at %s\n", workspace.LastSync, workspace.LastSync.Finished)
			}
			if workspace.WatchError != "" {
				fmt.Fprintf(writer, "Watcher: %s\n", workspace.WatchError)
			}
		}
		return nil
	})
}

func (client Client) open(ctx context.Context, writer io.Writer, workspace Workspace) error {
	if control(ctx, client.socket(workspace), "check") != nil {
		return errors.New("the worktree has no live tunnel. Run remote-dev up")
	}
	names := slices.Sorted(maps.Keys(workspace.RemotePorts))
	if len(names) == 0 || workspace.LocalPorts[names[0]] == 0 {
		return errors.New("the worktree has no forwarded application port")
	}
	address := fmt.Sprintf("http://%s:%d", loopback, workspace.LocalPorts[names[0]])
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	if err := exec.CommandContext(ctx, opener, address).Run(); err != nil {
		return fmt.Errorf("open %s manually: %w", address, err)
	}
	fmt.Fprintln(writer, address)
	return nil
}

func (client Client) workspaces() ([]Workspace, error) {
	records, err := filepath.Glob(filepath.Join(client.state, "workspace-*.json"))
	if err != nil {
		return nil, err
	}
	workspaces := make([]Workspace, 0, len(records))
	for _, path := range records {
		var workspace Workspace
		if err := readJSON(path, &workspace); err != nil {
			return nil, err
		}
		workspaces = append(workspaces, workspace)
	}
	return workspaces, nil
}

func (client Client) list(writer io.Writer) error {
	workspaces, err := client.workspaces()
	if err != nil {
		return err
	}
	if len(workspaces) == 0 {
		fmt.Fprintln(writer, "No Go workspaces yet. Run remote-dev up from a Git worktree.")
	}
	for _, workspace := range workspaces {
		fmt.Fprintf(writer, "%s  %s\n", workspace.Connection.Target, workspace.Root)
	}
	return nil
}

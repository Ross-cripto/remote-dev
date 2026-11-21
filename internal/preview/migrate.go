package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

var legacyKeyPattern = regexp.MustCompile(`^[0-9a-f]{24}$`)

type legacyWorkspace struct {
	Key         string         `json:"-"`
	Root        string         `json:"path"`
	Host        string         `json:"host"`
	Mode        string         `json:"mode"`
	RemotePorts map[string]int `json:"remote_ports"`
	LocalPorts  map[string]int `json:"local_ports"`
}

func findLegacyWorkspace(state, root, host string) (legacyWorkspace, error) {
	records, err := filepath.Glob(filepath.Join(state, "workspace-*.json"))
	if err != nil {
		return legacyWorkspace{}, err
	}
	var selected legacyWorkspace
	for _, filename := range records {
		var record legacyWorkspace
		if err := readJSON(filename, &record); err != nil {
			return selected, err
		}
		if record.Root != root || host != "" && record.Host != host {
			continue
		}
		if selected.Key != "" {
			return selected, errors.New("several Python sessions exist for this worktree. Select one with --host")
		}
		record.Key = strings.TrimSuffix(strings.TrimPrefix(filepath.Base(filename), "workspace-"), ".json")
		if !legacyKeyPattern.MatchString(record.Key) || record.Host == "" {
			return selected, errors.New("invalid Python workspace record")
		}
		selected = record
	}
	if selected.Key == "" {
		return selected, os.ErrNotExist
	}
	return selected, nil
}

func (record legacyWorkspace) native(connection Connection) Workspace {
	return Workspace{
		Root: record.Root, Key: record.Key, Connection: connection, Mode: record.Mode, HostOverride: record.Host,
		RemotePorts: record.RemotePorts, LocalPorts: record.LocalPorts, LegacyTunnel: true,
	}
}

func legacyWatcherActive(path string) (bool, error) {
	lease, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer lease.Close()
	err = syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

func (client Client) readyConnection(ctx context.Context, connection Connection) (Connection, error) {
	facts, err := connection.probe(ctx, false)
	if err != nil {
		return connection, err
	}
	if err := facts.ready(); err != nil {
		return connection, err
	}
	ready, _, err := connection.prepared(ctx, facts)
	return ready, err
}

func (client Client) refreshGoWorkspace(ctx context.Context, workspace *Workspace) error {
	connection, err := client.readyConnection(ctx, workspace.Connection)
	if err != nil {
		return err
	}
	workspace.Connection = connection
	return client.setWatcher(workspace, workspace.Lease != "")
}

func (client Client) migrate(ctx context.Context, writer io.Writer, root, host string) error {
	return client.underWorkspaceLock(root, func() error {
		existing, err := client.readWorkspace(root)
		if err == nil {
			if host != "" {
				requested, err := client.resolveConnection(host)
				if err != nil {
					return err
				}
				if requested.Target != existing.Connection.Target {
					return errors.New("this worktree already has a separate Go workspace on another host")
				}
			}
			if err := client.refreshGoWorkspace(ctx, &existing); err != nil {
				return err
			}
			fmt.Fprintln(writer, "Go workspace refreshed.")
			printLinks(writer, existing)
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		source := client.legacyState()
		previous, err := findLegacyWorkspace(source, root, host)
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("no Python or Go workspace recorded for this worktree. Run remote-dev up")
		}
		if err != nil {
			return err
		}
		connection, err := client.resolveConnection(previous.Host)
		if err != nil {
			return err
		}
		connection, err = client.readyConnection(ctx, connection)
		if err != nil {
			return err
		}
		return withLock(filepath.Join(source, "operation.lock"), func() error {
			record, err := findLegacyWorkspace(source, root, previous.Host)
			if err != nil {
				return err
			}
			if record.Key != previous.Key {
				return errors.New("the Python workspace changed during migration, retry")
			}
			workspace := record.native(connection)
			err = client.adoptLocked(ctx, source, &workspace)
			if err != nil {
				return err
			}
			fmt.Fprintln(writer, "Workspace adopted by Go.")
			if control(ctx, client.socket(workspace), "check") == nil {
				printLinks(writer, workspace)
			}
			return nil
		})
	})
}

func (client Client) adoptLocked(ctx context.Context, source string, workspace *Workspace) error {
	if _, err := workspace.Connection.host(ctx, "status", workspace.Key, hostRequest{}, nil); err != nil {
		return err
	}
	lease := filepath.Join(source, "watch-"+workspace.Key+".lease")
	return withLock(filepath.Join(source, "watch-"+workspace.Key+".lock"), func() error {
		watching, err := legacyWatcherActive(lease)
		if err != nil {
			return err
		}
		if err := client.setWatcher(workspace, watching); err != nil {
			return errors.Join(err, removeIfPresent(client.workspacePath(workspace.Root)))
		}
		if err := removeIfPresent(lease); err != nil {
			return errors.Join(err, removeIfPresent(client.workspacePath(workspace.Root)))
		}
		return nil
	})
}

func removeIfPresent(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

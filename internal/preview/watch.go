package preview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	saveDebounce     = 150 * time.Millisecond
	watchReconcile   = 5 * time.Second
	maxWatchErrorLen = 500
)

var errLeaseEnded = errors.New("watcher lease ended")

func (client Client) setWatcher(workspace *Workspace, enabled bool) error {
	workspace.Lease = ""
	workspace.WatchError = ""
	if !enabled {
		return client.saveWorkspace(*workspace)
	}
	lease, err := randomID()
	if err != nil {
		return err
	}
	workspace.Lease = lease
	if err := client.saveWorkspace(*workspace); err != nil {
		return err
	}

	executable, err := os.Executable()
	if err == nil {
		process := exec.Command(executable, "__watch", workspace.Root, lease)
		process.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		err = process.Start()
		if err == nil {
			go process.Wait()
			return nil
		}
	}
	workspace.Lease = ""
	return errors.Join(fmt.Errorf("start workspace watcher: %w", err), client.saveWorkspace(*workspace))
}

func watcherDirectories(workspace Workspace) map[string]bool {
	directories := map[string]bool{workspace.Root: true}
	for name := range workspace.Records {
		directory := filepath.Dir(filepath.Join(workspace.Root, name))
		for !directories[directory] {
			directories[directory] = true
			directory = filepath.Dir(directory)
		}
	}
	return directories
}

func refreshWatches(watcher *fsnotify.Watcher, workspace Workspace) {
	directories := watcherDirectories(workspace)
	for _, directory := range watcher.WatchList() {
		if !directories[directory] {
			watcher.Remove(directory)
		}
		delete(directories, directory)
	}
	for directory := range directories {
		// The reconciliation timer still detects changes when the OS watch limit is reached.
		watcher.Add(directory)
	}
}

func (client Client) reconcile(ctx context.Context, root, lease string) (Workspace, error) {
	var current Workspace
	err := client.existing(root, func(workspace *Workspace) error {
		if workspace.Lease != lease {
			return errLeaseEnded
		}
		_, syncErr := client.deploy(ctx, workspace, "sync", false)
		tunnelErr := client.ensureTunnel(ctx, workspace)
		current = *workspace
		message := ""
		if failure := errors.Join(syncErr, tunnelErr); failure != nil {
			message = strings.ReplaceAll(failure.Error(), "\n", " ")
			if len(message) > maxWatchErrorLen {
				message = message[:maxWatchErrorLen]
			}
		}
		if workspace.WatchError == message {
			return nil
		}
		workspace.WatchError = message
		return client.saveWorkspace(*workspace)
	})
	return current, err
}

func (client Client) watch(ctx context.Context, root, lease string) error {
	watcher, watcherErr := fsnotify.NewWatcher()
	var events <-chan fsnotify.Event
	var failures <-chan error
	if watcherErr == nil {
		defer watcher.Close()
		events = watcher.Events
		failures = watcher.Errors
	}

	reconcile := func() error {
		workspace, err := client.reconcile(ctx, root, lease)
		if err != nil {
			return err
		}
		if watcherErr == nil {
			refreshWatches(watcher, workspace)
		}
		return nil
	}
	if err := reconcile(); err != nil {
		if errors.Is(err, errLeaseEnded) {
			return nil
		}
		return err
	}

	debounce := time.NewTimer(saveDebounce)
	debounce.Stop()
	defer debounce.Stop()
	ticker := time.NewTicker(watchReconcile)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, open := <-events:
			if !open {
				events = nil
				continue
			}
			debounce.Reset(saveDebounce)
			continue
		case _, open := <-failures:
			if !open {
				failures = nil
				continue
			}
		case <-debounce.C:
		case <-ticker.C:
		}
		if err := reconcile(); err != nil {
			if errors.Is(err, errLeaseEnded) {
				return nil
			}
			return err
		}
	}
}

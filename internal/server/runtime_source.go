package server

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

func runtimeSource(workspace string) string {
	return filepath.Join(workspace, "runtime-source")
}

func applyRuntimeSource(workspace, previous, desired string) ([]string, []string, error) {
	runtime := runtimeSource(workspace)
	if _, err := workspacePath(workspace, "runtime-source"); err != nil {
		return nil, nil, err
	}
	if err := os.Mkdir(runtime, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, nil, err
	}
	if err := os.Chmod(runtime, 0o755); err != nil {
		return nil, nil, err
	}
	before, err := baselineInventory(previous)
	if err != nil {
		return nil, nil, err
	}
	after, err := readSnapshotInventory(desired)
	if err != nil {
		return nil, nil, err
	}
	current, err := inventory(runtime)
	if err != nil {
		return nil, nil, err
	}
	if err := verifyRuntimeChanges(before, after, current); err != nil {
		return nil, nil, err
	}
	changed, deleted := inventoryChanges(before, after)
	if err := updateRuntimeFiles(workspace, runtime, desired, current, changed, deleted); err != nil {
		return nil, nil, err
	}
	final, err := inventory(runtime)
	if err != nil {
		return nil, nil, err
	}
	if !maps.Equal(final, after) {
		return nil, nil, errors.New("runtime source did not match the prepared revision. Retry sync")
	}
	return changed, deleted, nil
}

func baselineInventory(source string) (fileInventory, error) {
	if source == "" {
		return fileInventory{}, nil
	}
	return readSnapshotInventory(source)
}

func verifyRuntimeChanges(before, after, current fileInventory) error {
	names := map[string]bool{}
	for _, inventory := range []fileInventory{before, after, current} {
		for name := range inventory {
			names[name] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		if current[name] != before[name] && current[name] != after[name] {
			return fmt.Errorf("remote source changed at %s. Preserve that edit before syncing", name)
		}
	}
	return nil
}

func inventoryChanges(before, after fileInventory) (changed, deleted []string) {
	for name, digest := range after {
		if before[name] != digest {
			changed = append(changed, name)
		}
	}
	for name := range before {
		if _, exists := after[name]; !exists {
			deleted = append(deleted, name)
		}
	}
	slices.Sort(changed)
	slices.Sort(deleted)
	return changed, deleted
}

func updateRuntimeFiles(workspace, runtime, desired string, current fileInventory, changed, deleted []string) error {
	staged, err := os.MkdirTemp(workspace, "source-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staged)
	for _, name := range changed {
		if err := stageRuntimeFile(staged, desired, name); err != nil {
			return err
		}
	}
	for _, name := range deleted {
		if _, exists := current[name]; !exists {
			continue
		}
		target, err := workspacePath(runtime, name)
		if err != nil {
			return err
		}
		if err := removeRegularFile(target); err != nil {
			return err
		}
	}
	for _, name := range changed {
		if err := publishRuntimeFile(staged, runtime, name); err != nil {
			return err
		}
	}
	return nil
}

func stageRuntimeFile(staged, desired, name string) error {
	incoming, err := workspacePath(staged, name)
	if err != nil {
		return err
	}
	source, err := workspacePath(desired, name)
	if err != nil {
		return err
	}
	if err := createSourceParents(staged, incoming); err != nil {
		return err
	}
	return copySourceFile(source, incoming)
}

func publishRuntimeFile(staged, runtime, name string) error {
	target, err := workspacePath(runtime, name)
	if err != nil {
		return err
	}
	info, err := os.Lstat(target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && info.IsDir() {
		if err := removeEmptyTree(target); err != nil {
			return err
		}
	}
	if err := createSourceParents(runtime, target); err != nil {
		return err
	}
	return os.Rename(filepath.Join(staged, filepath.FromSlash(name)), target)
}

func removeEmptyTree(directory string) error {
	var directories []string
	if err := filepath.WalkDir(directory, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return fmt.Errorf("cannot replace nonempty directory: %s", directory)
		}
		directories = append(directories, filename)
		return nil
	}); err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err := os.Remove(directories[index]); err != nil {
			return err
		}
	}
	return nil
}

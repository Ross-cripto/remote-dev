package server

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
)

const (
	liveSourceLabel = "io.remote-dev.live-source"
	reloadLabel     = "io.remote-dev.reload"
	nodeSource      = "/workspace"
)

var dependencyInputs = map[string]bool{
	"package.json": true, "package-lock.json": true, "npm-shrinkwrap.json": true,
	"pnpm-lock.yaml": true, "pnpm-workspace.yaml": true, "yarn.lock": true,
	"bun.lock": true, "bun.lockb": true, "Dockerfile": true, "requirements.txt": true,
	"pyproject.toml": true, "uv.lock": true,
}

func workspaceBinds(source string, service map[string]any) []string {
	var paths []string
	for _, raw := range list(service["volumes"]) {
		volume := object(raw)
		filename := setting(volume["source"])
		if volume["type"] == "bind" && fileWithin(source, filename) {
			paths = append(paths, filepath.Clean(filename))
		}
	}
	return paths
}

func fileWithin(root, filename string) bool {
	relative, err := filepath.Rel(root, filename)
	return err == nil && filepath.IsLocal(relative)
}

func readsFilesOnRequest(service map[string]any) bool {
	switch imageName(service) {
	case "nginx", "httpd", "caddy":
		return true
	}
	if setting(service["command"]) == "http.server" {
		return true
	}
	for _, argument := range list(service["command"]) {
		if setting(argument) == "http.server" {
			return true
		}
	}
	return false
}

func usesNodeSource(service map[string]any) bool {
	return object(service["labels"])[liveSourceLabel] == nodeSource
}

func watchesSource(service map[string]any) bool {
	return object(service["labels"])[reloadLabel] == "watch"
}

func servicesToReload(source string, config composeSpec, containers map[string][]string, files []string) []string {
	var restart []string
	for _, name := range slices.Sorted(maps.Keys(config.services)) {
		service := config.services[name]
		if len(containers[name]) == 0 || watchesSource(service) || readsFilesOnRequest(service) {
			continue
		}
		if mountedFileChanged(workspaceBinds(source, service), files) {
			restart = append(restart, name)
		}
	}
	return restart
}

func mountedFileChanged(mounts, files []string) bool {
	for _, mount := range mounts {
		for _, filename := range files {
			if fileWithin(mount, filename) {
				return true
			}
		}
	}
	return false
}

func dependencyChanged(modified []string) bool {
	for _, name := range modified {
		if dependencyInputs[path.Base(name)] {
			return true
		}
	}
	return false
}

func (host hostCommand) refreshServices(command []string, source string, config composeSpec, containers map[string][]string, changed, deleted []string) error {
	modified := append(slices.Clone(changed), deleted...)
	if len(modified) == 0 {
		return nil
	}
	if config.usesBuild() {
		return host.composeUp(command, source, true, false, nil)
	}
	serviceNames := slices.Sorted(maps.Keys(config.services))
	var generated []string
	for _, name := range serviceNames {
		if usesNodeSource(config.services[name]) {
			generated = append(generated, name)
		}
	}
	if len(generated) > 0 && dependencyChanged(modified) {
		return host.composeUp(command, source, false, true, generated)
	}
	var remount []string
	files := make([]string, 0, len(modified))
	for _, name := range modified {
		files = append(files, filepath.Join(source, filepath.FromSlash(name)))
	}
	for _, name := range serviceNames {
		if len(containers[name]) > 0 && bindsIndividualFile(workspaceBinds(source, config.services[name]), files) {
			remount = append(remount, name)
		}
	}
	if len(remount) > 0 {
		if err := host.composeUp(command, source, false, true, remount); err != nil {
			return err
		}
		for _, name := range remount {
			delete(containers, name)
		}
	}
	if err := host.updateNodeContainers(source, config, containers, changed, deleted); err != nil {
		return err
	}
	restart := servicesToReload(source, config, containers, files)
	if len(restart) != 0 {
		_, err := host.execute(source, nil, true, append(append(slices.Clone(command), "restart"), restart...)...)
		return err
	}
	return nil
}

func bindsIndividualFile(mounts, files []string) bool {
	for _, mount := range mounts {
		if slices.Contains(files, mount) {
			return true
		}
	}
	return false
}

func (host hostCommand) updateNodeContainers(source string, config composeSpec, containers map[string][]string, changed, deleted []string) error {
	var watching []string
	for _, name := range slices.Sorted(maps.Keys(config.services)) {
		service := config.services[name]
		if usesNodeSource(service) && watchesSource(service) && len(containers[name]) > 0 {
			watching = append(watching, name)
		}
	}
	if len(watching) == 0 {
		return nil
	}
	payload, err := os.CreateTemp("", "remote-dev-node-update-")
	if err != nil {
		return err
	}
	defer os.Remove(payload.Name())
	defer payload.Close()
	archive := tar.NewWriter(payload)
	for _, name := range changed {
		if err := addRuntimeFile(archive, source, name); err != nil {
			return err
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	for _, name := range watching {
		for _, container := range containers[name] {
			if err := host.updateNodeContainer(payload, container, changed, deleted); err != nil {
				return fmt.Errorf("could not update source for %s. Retry sync: %w", name, err)
			}
		}
	}
	return nil
}

func (host hostCommand) updateNodeContainer(payload *os.File, container string, changed, deleted []string) error {
	for _, name := range deleted {
		if _, err := host.docker("", "exec", container, "rm", "-f", "--", nodeSource+"/"+name); err != nil {
			return err
		}
	}
	if len(changed) == 0 {
		return nil
	}
	if _, err := payload.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err := host.execute("", payload, true, "cp", "-", container+":"+nodeSource)
	return err
}

func addRuntimeFile(archive *tar.Writer, source, name string) error {
	filename, err := workspacePath(source, name)
	if err != nil {
		return err
	}
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("unsupported source file: " + name)
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = name
	if err := archive.WriteHeader(header); err != nil {
		return err
	}
	_, err = io.Copy(archive, file)
	return err
}

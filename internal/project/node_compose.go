package project

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	shellSafeWord    = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)
	pinnedManager    = regexp.MustCompile(`^(npm|pnpm|yarn)@([0-9]+)\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.+-]+)?$`)
	nodeVersion      = regexp.MustCompile(`^v?(20|22|24)(?:\.[0-9]+){0,2}$`)
	managerLockfiles = []struct {
		Name  string
		Files []string
	}{
		{"npm", []string{"package-lock.json", "npm-shrinkwrap.json"}},
		{"pnpm", []string{"pnpm-lock.yaml"}},
		{"yarn", []string{"yarn.lock"}},
		{"bun", []string{"bun.lock", "bun.lockb"}},
	}
)

func nodeCompose(root string, rootPackage *nodePackage, patterns []string, apps []nodeApp, runAtRoot bool) (composeDocument, []Forward, error) {
	document := composeDocument{Services: make(map[string]composeService)}
	var forwards []Forward
	if runAtRoot {
		service, err := detectedNodeService(root, *rootPackage, *rootPackage, apps, "dev", nil)
		if err != nil {
			return composeDocument{}, nil, err
		}
		document.Services["web"] = service
		for _, app := range apps {
			forwards = append(forwards, Forward{Name: app.Name, Service: "web", Target: app.Port})
		}
		return document, forwards, nil
	}
	for _, app := range apps {
		installPackage := app.Package
		if len(patterns) > 0 && rootPackage != nil {
			installPackage = *rootPackage
		}
		service, err := detectedNodeService(root, app.Package, installPackage, []nodeApp{app}, app.Script, app.Arguments)
		if err != nil {
			return composeDocument{}, nil, err
		}
		document.Services[app.Name] = service
		forwards = append(forwards, Forward{Name: app.Name, Service: app.Name, Target: app.Port})
	}
	return document, forwards, nil
}

func detectedNodeService(root string, appPackage, installPackage nodePackage, apps []nodeApp, script string, arguments []string) (composeService, error) {
	var ports []int
	watches := true
	for _, app := range apps {
		ports = append(ports, app.Port)
		watches = watches && app.Watches
	}
	environment := map[string]string{"HOST": "0.0.0.0"}
	if len(ports) == 1 {
		environment["PORT"] = strconv.Itoa(ports[0])
	}
	service := newComposeService(appPackage.Directory, nodeSource, environment, ports)
	service.Labels = nodeLabels(watches)
	return configureDetectedNodeService(root, appPackage, installPackage, script, arguments, service)
}

func configureDetectedNodeService(root string, appPackage, installPackage nodePackage, script string, arguments []string, service composeService) (composeService, error) {
	manager, err := detectPackageManager(root, installPackage)
	if err != nil {
		return composeService{}, err
	}
	image, err := detectNodeImage(root, installPackage)
	if err != nil {
		return composeService{}, err
	}

	runDirectory := path.Join(workspace, appPackage.Directory)
	installDirectory := path.Join(workspace, installPackage.Directory)
	startup := []string{"cp -a " + nodeSource + "/. " + workspace, "cd " + shellQuote(installDirectory)}
	if manager.Setup != "" {
		startup = append(startup, manager.Setup)
	}
	startup = append(startup, manager.Install)
	if runDirectory != installDirectory {
		startup = append(startup, "cd "+shellQuote(runDirectory))
	}
	run := "exec " + manager.Name + " run " + shellQuote(script)
	if len(arguments) > 0 && manager.Name == "npm" {
		run += " --"
	}
	for _, argument := range arguments {
		run += " " + shellQuote(argument)
	}
	startup = append(startup, run)

	service.Image = image
	service.Command = []string{"/bin/sh", "-c", escapeCompose(strings.Join(startup, " && "))}
	service.Volumes = append(service.Volumes, composeVolume{
		Type: "volume", Target: escapeCompose(path.Join(installDirectory, "node_modules")),
	})
	return service, nil
}

func shellQuote(word string) string {
	if shellSafeWord.MatchString(word) {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'"'"'`) + "'"
}

type packageManager struct {
	Name    string
	Setup   string
	Install string
}

func detectPackageManager(root string, appPackage nodePackage) (packageManager, error) {
	lockfiles, err := detectManagerLockfiles(root, appPackage.Directory)
	if err != nil {
		return packageManager{}, err
	}
	name := ""
	var pinned []string
	if declaration := appPackage.Manifest["packageManager"]; declaration != nil {
		text, _ := declaration.(string)
		pinned = pinnedManager.FindStringSubmatch(text)
		if pinned == nil {
			return packageManager{}, fmt.Errorf("unsupported packageManager in %s. Use %s for custom commands", path.Join(appPackage.Directory, "package.json"), ConfigFile)
		}
		name = pinned[1]
	} else {
		switch len(lockfiles) {
		case 0:
			pnpmWorkspace, err := projectFile(root, path.Join(appPackage.Directory, pnpmWorkspaceFile))
			if err != nil {
				return packageManager{}, err
			}
			name = "npm"
			if pnpmWorkspace {
				name = "pnpm"
			}
		case 1:
			for manager := range lockfiles {
				name = manager
			}
		default:
			return packageManager{}, fmt.Errorf("several package manager lockfiles found in %s. Declare packageManager in package.json", appPackage.Directory)
		}
	}
	manager := packageManager{Name: name}
	hasLock := lockfiles[name]
	switch name {
	case "npm":
		if pinned != nil {
			manager.Setup = "corepack enable npm"
		}
		manager.Install = "npm install --no-package-lock"
		if hasLock {
			manager.Install = "npm ci"
		}
	case "pnpm":
		manager.Setup = "corepack enable pnpm"
		manager.Install = "pnpm install --no-frozen-lockfile"
		if hasLock {
			manager.Install = "pnpm install --frozen-lockfile"
		}
	case "yarn":
		manager.Setup = "corepack enable yarn"
		manager.Install = "yarn install"
		if hasLock {
			manager.Install = "yarn install --frozen-lockfile"
			if pinned != nil && isModernYarn(pinned[2]) {
				manager.Install = "yarn install --immutable"
			}
		}
	default:
		return packageManager{}, fmt.Errorf("unsupported package manager %q. Use %s for custom commands", name, ConfigFile)
	}
	return manager, nil
}

func isModernYarn(version string) bool {
	version = strings.TrimLeft(version, "0")
	return version != "" && version != "1"
}

func detectManagerLockfiles(root, directory string) (map[string]bool, error) {
	found := make(map[string]bool)
	for _, manager := range managerLockfiles {
		for _, filename := range manager.Files {
			exists, err := projectFile(root, path.Join(directory, filename))
			if err != nil {
				return nil, err
			}
			if exists {
				found[manager.Name] = true
				break
			}
		}
	}
	return found, nil
}

func detectNodeImage(root string, appPackage nodePackage) (string, error) {
	directories := []string{appPackage.Directory}
	if appPackage.Directory != "." {
		directories = append(directories, ".")
	}
	for _, directory := range directories {
		for _, filename := range []string{".nvmrc", ".node-version"} {
			relative := path.Join(directory, filename)
			exists, err := projectFile(root, relative)
			if err != nil {
				return "", err
			}
			if !exists {
				continue
			}
			content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
			if err != nil {
				return "", fmt.Errorf("read %s: %w", relative, err)
			}
			version := nodeVersion.FindStringSubmatch(strings.TrimSpace(string(content)))
			if version != nil {
				return fmt.Sprintf("node:%s-bookworm-slim", version[1]), nil
			}
		}
	}
	return defaultNodeImage, nil
}

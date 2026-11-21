package project

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/google/shlex"
	"go.yaml.in/yaml/v3"
)

const (
	pnpmWorkspaceFile = "pnpm-workspace.yaml"
	vitePort          = 5173
	defaultNodePort   = 3000
)

var (
	conventionalApps      = []string{"frontend", "backend", "web", "api", "server", "app", "apps/*"}
	workspacePattern      = regexp.MustCompile(`^[A-Za-z0-9_.*?/-]+$`)
	nodeServiceSeparators = regexp.MustCompile(`[^a-z0-9_-]+`)
	declaredPort          = regexp.MustCompile(`^[0-9]{1,5}$`)
)

type nodePackage struct {
	Directory string
	Manifest  map[string]any
}

func (appPackage nodePackage) script(name string) string {
	scripts, _ := appPackage.Manifest["scripts"].(map[string]any)
	command, _ := scripts[name].(string)
	return command
}

type nodeApp struct {
	Package                 nodePackage
	Name                    string
	Script                  string
	Port                    int
	Arguments               []string
	Watches                 bool
	ReadyForWorkspaceRunner bool
}

func readNodePackage(root, directory string) (*nodePackage, error) {
	relative := path.Join(directory, "package.json")
	found, err := projectFile(root, relative)
	if err != nil || !found {
		return nil, err
	}
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", relative, err)
	}
	var manifest any
	if err := json.Unmarshal(content, &manifest); err != nil {
		return nil, fmt.Errorf("invalid JSON in %s: %w", relative, err)
	}
	object, ok := manifest.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected a JSON object in %s", relative)
	}
	return &nodePackage{Directory: directory, Manifest: object}, nil
}

func workspacePatterns(root string, rootPackage *nodePackage) ([]string, error) {
	found, err := projectFile(root, pnpmWorkspaceFile)
	if err != nil {
		return nil, err
	}
	if found {
		return pnpmPatterns(root)
	}
	if rootPackage == nil {
		return nil, nil
	}
	workspaces, present := rootPackage.Manifest["workspaces"]
	if !present {
		return nil, nil
	}
	if config, isObject := workspaces.(map[string]any); isObject {
		workspaces, present = config["packages"]
		if !present {
			return nil, nil
		}
	}
	patterns, ok := stringList(workspaces)
	if !ok {
		return nil, fmt.Errorf("expected a list of workspace paths in package.json. Use %s to select an app", ConfigFile)
	}
	return patterns, nil
}

func pnpmPatterns(root string) ([]string, error) {
	content, err := os.ReadFile(filepath.Join(root, pnpmWorkspaceFile))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", pnpmWorkspaceFile, err)
	}
	var config map[string]any
	if err := yaml.Unmarshal(content, &config); err != nil {
		return nil, fmt.Errorf("read %s: %w. Select an app in %s", pnpmWorkspaceFile, err, ConfigFile)
	}
	packages, present := config["packages"]
	if !present || packages == nil {
		return nil, nil
	}
	patterns, ok := stringList(packages)
	if !ok {
		return nil, fmt.Errorf("expected a packages list in %s. Select an app in %s", pnpmWorkspaceFile, ConfigFile)
	}
	return patterns, nil
}

func stringList(value any) ([]string, bool) {
	list, ok := value.([]any)
	if !ok {
		return nil, false
	}
	var textValues []string
	for _, item := range list {
		text, ok := item.(string)
		if !ok {
			return nil, false
		}
		textValues = append(textValues, text)
	}
	return textValues, true
}

func workspaceDirectories(root string, patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		patterns = conventionalApps
	}
	directories := make(map[string]bool)
	excluded := make(map[string]bool)
	for _, glob := range patterns {
		pattern := strings.TrimPrefix(glob, "!")
		if !supportedWorkspacePattern(pattern) {
			return nil, fmt.Errorf("workspace path pattern %q needs an explicit app selection in %s", glob, ConfigFile)
		}
		target := directories
		if glob != pattern {
			target = excluded
		}
		matches, err := fs.Glob(os.DirFS(root), path.Clean(pattern))
		if err != nil {
			return nil, fmt.Errorf("expand workspace path %q: %w", glob, err)
		}
		for _, match := range matches {
			if workspaceDirectory(root, match) {
				target[match] = true
			}
		}
	}
	for directory := range excluded {
		delete(directories, directory)
	}
	return slices.Sorted(maps.Keys(directories)), nil
}

func supportedWorkspacePattern(pattern string) bool {
	if path.IsAbs(pattern) || strings.Contains(pattern, "**") || !workspacePattern.MatchString(pattern) {
		return false
	}
	for _, part := range strings.Split(pattern, "/") {
		if part == ".." || (part != "." && strings.HasPrefix(part, ".")) || part == "node_modules" {
			return false
		}
	}
	return true
}

func workspaceDirectory(root, relative string) bool {
	if relative == "." {
		return false
	}
	filename := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Stat(filename)
	if err != nil || !info.IsDir() {
		return false
	}
	canonical, err := filepath.EvalSymlinks(filename)
	return err == nil && canonical == filename
}

func detectNodeApps(root string, rootPackage *nodePackage, patterns []string) ([]nodeApp, error) {
	if rootPackage != nil {
		app, err := detectNodeApp(*rootPackage, "web")
		if err != nil {
			return nil, err
		}
		if app != nil {
			return []nodeApp{*app}, nil
		}
	}
	directories, err := workspaceDirectories(root, patterns)
	if err != nil {
		return nil, err
	}
	var apps []nodeApp
	names := make(map[string]bool)
	for _, directory := range directories {
		appPackage, err := readNodePackage(root, directory)
		if err != nil {
			return nil, err
		}
		if appPackage == nil {
			continue
		}
		name := strings.Trim(nodeServiceSeparators.ReplaceAllString(strings.ToLower(directory), "-"), "-_")
		if _, err := validateName(name, "service name"); err != nil {
			return nil, err
		}
		app, err := detectNodeApp(*appPackage, name)
		if err != nil {
			return nil, err
		}
		if app == nil {
			continue
		}
		if names[name] {
			return nil, fmt.Errorf("several detected apps use the service name %q. Select apps in %s", name, ConfigFile)
		}
		names[name] = true
		apps = append(apps, *app)
	}
	return apps, nil
}

func detectNodeApp(appPackage nodePackage, name string) (*nodeApp, error) {
	for _, script := range []string{"dev", "start:dev"} {
		arguments, environment := parseDevCommand(appPackage.script(script))
		if len(arguments) == 0 || !supportedDevCommand(arguments[0], arguments[1:]) {
			continue
		}
		tool, options := arguments[0], arguments[1:]
		port, err := nodePort(tool, options, environment)
		if err != nil {
			return nil, err
		}
		extra, ready := nodeArguments(tool, options, port)
		return &nodeApp{Package: appPackage, Name: name, Script: script, Port: port, Arguments: extra, Watches: nodeCommandWatches(arguments), ReadyForWorkspaceRunner: ready}, nil
	}
	return nil, nil
}

func supportedDevCommand(tool string, options []string) bool {
	switch tool {
	case "vite":
		return startsVite(options)
	case "next":
		return startsWith(options, "dev")
	case "nest":
		return startsWith(options, "start")
	case "node", "tsx", "nodemon":
		return len(options) > 0
	default:
		return false
	}
}

func nodeArguments(tool string, options []string, port int) ([]string, bool) {
	var extra []string
	ready := true
	switch tool {
	case "vite":
		host, explicit := optionValue(options, "--host")
		ready = explicit && slices.Contains([]string{"", "0.0.0.0", "::"}, host)
		if !ready {
			extra = append(extra, "--host", "0.0.0.0")
		}
		if _, hasPort := optionValue(options, "--port"); !hasPort {
			extra = append(extra, "--port", strconv.Itoa(port))
		}
		if !slices.Contains(options, "--strictPort") {
			extra = append(extra, "--strictPort")
		}
	case "next":
		host, explicit := optionValue(options, "--hostname", "-H")
		ready = !explicit || host == "0.0.0.0" || host == "::"
		if !ready {
			extra = append(extra, "--hostname", "0.0.0.0")
		}
	}
	return extra, ready
}

func nodePort(tool string, options []string, environment map[string]string) (int, error) {
	var value string
	var present bool
	switch tool {
	case "next":
		value, present = optionValue(options, "--port", "-p")
	case "nest":
	default:
		value, present = optionValue(options, "--port")
	}
	if !present && tool != "vite" {
		value, present = environment["PORT"]
	}
	if !present {
		if tool == "vite" {
			return vitePort, nil
		}
		return defaultNodePort, nil
	}
	port, err := strconv.ParseInt(value, 10, 64)
	if err != nil || !declaredPort.MatchString(value) || !validTCPPort(port) {
		return 0, fmt.Errorf("cannot determine the port from the Node dev script. Set it in %s", ConfigFile)
	}
	return int(port), nil
}

func optionValue(options []string, names ...string) (string, bool) {
	for index := len(options) - 1; index >= 0; index-- {
		flag, value, hasValue := strings.Cut(options[index], "=")
		if !slices.Contains(names, flag) {
			continue
		}
		if hasValue {
			return value, true
		}
		if index+1 < len(options) && !strings.HasPrefix(options[index+1], "-") {
			return options[index+1], true
		}
		return "", true
	}
	return "", false
}

func startsWith(options []string, expected string) bool {
	return len(options) > 0 && options[0] == expected
}

func startsVite(options []string) bool {
	return len(options) == 0 || strings.HasPrefix(options[0], "-") || slices.Contains([]string{"dev", "serve"}, options[0])
}

func nodeCommandWatches(arguments []string) bool {
	if len(arguments) == 0 {
		return false
	}
	options := arguments[1:]
	switch arguments[0] {
	case "nodemon":
		return true
	case "vite":
		return startsVite(options)
	case "next":
		return startsWith(options, "dev")
	case "tsx":
		return startsWith(options, "watch")
	case "nest":
		return startsWith(options, "start") && (slices.Contains(options, "--watch") || slices.Contains(options, "-w"))
	case "node":
		return slices.Contains(options, "--watch")
	default:
		return false
	}
}

func manifestScriptWatches(packageRoot, script string) bool {
	appPackage, err := readNodePackage(packageRoot, ".")
	if err != nil || appPackage == nil {
		return false
	}
	arguments, _ := parseDevCommand(appPackage.script(script))
	return nodeCommandWatches(arguments)
}

func parseDevCommand(script string) ([]string, map[string]string) {
	if strings.ContainsRune(script, '\n') {
		return nil, nil
	}
	singleCommand, usable := shellCommandPrefix(script)
	if !usable {
		return nil, nil
	}
	arguments, err := shlex.Split(singleCommand)
	if err != nil {
		return nil, nil
	}
	environment := make(map[string]string)
	for index, argument := range arguments {
		name, value, assigned := strings.Cut(argument, "=")
		if !assigned || !environmentName.MatchString(name) {
			return arguments[index:], environment
		}
		environment[name] = value
	}
	return nil, environment
}

func shellCommandPrefix(script string) (string, bool) {
	var quote rune
	escaped := false
	for index, character := range script {
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			}
			continue
		}
		switch character {
		case '\'', '"':
			quote = character
		case '#':
			return script[:index], true
		case ';', '&', '|', '(', ')', '<', '>':
			return "", false
		}
	}
	return script, true
}

func canRunWorkspaceScript(rootPackage *nodePackage, patterns []string, apps []nodeApp) bool {
	if rootPackage == nil || len(patterns) == 0 || apps[0].Package.Directory == "." {
		return false
	}
	ports := make(map[int]bool)
	for _, app := range apps {
		if app.Script != "dev" || !app.ReadyForWorkspaceRunner || ports[app.Port] {
			return false
		}
		ports[app.Port] = true
	}
	arguments, _ := parseDevCommand(rootPackage.script("dev"))
	return isWorkspaceRunner(arguments, len(apps))
}

func isWorkspaceRunner(arguments []string, appCount int) bool {
	if slices.Equal(arguments, []string{"turbo", "dev"}) || slices.Equal(arguments, []string{"turbo", "run", "dev"}) {
		return true
	}
	if len(arguments) < 4 {
		return false
	}
	if slices.Equal(arguments[:3], []string{"npm", "run", "dev"}) {
		options := arguments[3:]
		return appCount == 1 && slices.Contains(options, "--workspaces") && onlyOptions(options, "--workspaces", "--if-present")
	}
	if arguments[0] != "pnpm" || !slices.Equal(arguments[len(arguments)-2:], []string{"run", "dev"}) {
		return false
	}
	options := arguments[1 : len(arguments)-2]
	recursive := slices.Contains(options, "-r") || slices.Contains(options, "--recursive")
	parallel := appCount == 1 || slices.Contains(options, "--parallel")
	return recursive && parallel && onlyOptions(options, "-r", "--recursive", "--parallel")
}

func onlyOptions(options []string, allowed ...string) bool {
	for _, option := range options {
		if !slices.Contains(allowed, option) {
			return false
		}
	}
	return true
}

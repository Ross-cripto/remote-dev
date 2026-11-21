package project

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestComposePrefersDevelopmentOverride(t *testing.T) {
	root := projectFixture(t, map[string]string{
		"compose.yaml":      "services:\n  web:\n    build: .\n",
		"compose.dev.yaml":  "services:\n  web:\n    ports: [3000]\n",
		"compose.prod.yaml": "services:\n  web:\n    ports: [443]\n",
		"package.json":      "invalid JSON",
	})
	project, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if project.Mode != "compose" || project.ComposeFile != "compose.yaml" ||
		project.Override != "compose.dev.yaml" || !project.AutoPorts || len(project.Generated) != 0 {
		t.Fatalf("unexpected Compose project: %+v", project)
	}
}

func TestProjectDetectionRequiresExplicitSelection(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		message string
	}{
		{"several development files", map[string]string{"compose.dev.yaml": "", "compose.development.yml": ""}, "compose.dev.yaml, compose.development.yml"},
		{"production only", map[string]string{"compose.prod.yaml": ""}, "remote-dev.toml"},
		{"custom launcher", map[string]string{"package.json": `{"scripts":{"dev":"vite && custom-run"}}`}, "commands"},
		{"hybrid collision", map[string]string{"compose.yaml": "services:\n  web:\n    image: postgres:16\n", "package.json": `{"scripts":{"dev":"vite"}}`}, "overlap"},
		{"several lockfiles", map[string]string{"package.json": `{"scripts":{"dev":"vite"}}`, "package-lock.json": "", "pnpm-lock.yaml": ""}, "packageManager"},
		{"invalid port", map[string]string{"package.json": `{"scripts":{"dev":"vite --port 65536"}}`}, "port"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := Load(projectFixture(t, testCase.files))
			if err == nil || !strings.Contains(err.Error(), testCase.message) {
				t.Fatalf("expected error containing %q, got %v", testCase.message, err)
			}
		})
	}
}

func TestDetectionPreservesComposeControlForCustomServices(t *testing.T) {
	for _, composeText := range []string{
		"services:\n  frontend:\n    image: example/frontend:dev\n",
		"\"include\": [frontend.yaml]\nservices:\n  db:\n    image: postgres:16\n",
		"services:\n  db:\n    image: postgres:16\n    profiles: [dev]\n",
	} {
		root := projectFixture(t, map[string]string{
			"compose.yaml": composeText,
			"package.json": "invalid JSON",
		})
		project, err := Load(root)
		if err != nil || project.Mode != "compose" || len(project.Generated) != 0 {
			t.Fatalf("unexpected project for %q: %+v, %v", composeText, project, err)
		}
	}
}

func TestHybridViteUsesLoopbackAndAnAnonymousDependencyVolume(t *testing.T) {
	root := projectFixture(t, map[string]string{
		"compose.yaml":   "services:\n  database:\n    image: postgres:16 # Development database\n    volumes:\n      - web-node-modules:/var/lib/postgresql/data\nvolumes:\n  web-node-modules: {}\n",
		"package.json":   `{"scripts":{"dev":"vite --port 4201"},"packageManager":"pnpm@10.11.0"}`,
		"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
	})
	loadedProject, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if loadedProject.Mode != "node" || loadedProject.ComposeFile != "compose.yaml" || !loadedProject.AutoPorts ||
		!slices.Equal(loadedProject.Forwards, []Forward{{Name: "web", Service: "web", Target: 4201}}) {
		t.Fatalf("unexpected hybrid project: %+v", loadedProject)
	}
	generated := decodeGenerated(t, loadedProject)
	service := generated.Services["web"]
	if len(generated.Services) != 1 || len(generated.Volumes) != 0 ||
		service.Ports[0].HostIP != "127.0.0.1" || service.Ports[0].Target != 4201 ||
		service.Volumes[0].Source != "." || !service.Volumes[0].ReadOnly ||
		service.Volumes[1].Source != "" || service.Volumes[1].Target != "/workspace/node_modules" ||
		service.Labels["io.remote-dev.live-source"] != "/workspace" || service.Labels["io.remote-dev.reload"] != "watch" ||
		!strings.Contains(service.Command[2], "pnpm install --frozen-lockfile") ||
		!strings.Contains(service.Command[2], "pnpm run dev --host 0.0.0.0 --strictPort") {
		t.Fatalf("unexpected generated services: %+v", generated)
	}
}

func TestDetectedNodeRuntimes(t *testing.T) {
	cases := []struct {
		name   string
		files  map[string]string
		port   int
		image  string
		reload string
		steps  []string
	}{
		{"Yarn", map[string]string{"package.json": `{"scripts":{"dev":"vite"},"packageManager":"yarn@4.9.1"}`, "yarn.lock": ""}, 5173, "node:24-bookworm-slim", "watch", []string{"yarn install --immutable", "yarn run dev --host"}},
		{"generic Node", map[string]string{"package.json": `{"scripts":{"dev":"PORT=8080 node server.js"}}`, "package-lock.json": "", ".nvmrc": "v22.14.0\n"}, 8080, "node:22-bookworm-slim", "restart", []string{"npm ci", "exec npm run dev"}},
		{"Nest", map[string]string{"package.json": `{"scripts":{"start:dev":"nest start -w -p tsconfig.local.json"}}`}, 3000, "node:24-bookworm-slim", "watch", []string{"exec npm run start:dev"}},
		{"Next", map[string]string{"package.json": `{"scripts":{"dev":"next dev -p 4900 --hostname=127.0.0.1"}}`}, 4900, "node:24-bookworm-slim", "watch", []string{"npm run dev -- --hostname 0.0.0.0"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			loadedProject, err := Load(projectFixture(t, testCase.files))
			if err != nil {
				t.Fatal(err)
			}
			service := decodeGenerated(t, loadedProject).Services["web"]
			if len(loadedProject.Forwards) != 1 || loadedProject.Forwards[0].Target != testCase.port ||
				service.Image != testCase.image || service.Labels["io.remote-dev.reload"] != testCase.reload {
				t.Fatalf("unexpected project %+v or service %+v", loadedProject, service)
			}
			for _, step := range testCase.steps {
				if !strings.Contains(service.Command[2], step) {
					t.Errorf("expected command to contain %q, got %v", step, service.Command)
				}
			}
		})
	}
}

func TestWorkspaceInstallAndRunner(t *testing.T) {
	root := projectFixture(t, map[string]string{
		"package.json":               `{"scripts":{"dev":"pnpm -r --parallel run dev"},"packageManager":"pnpm@10.11.0"}`,
		"apps/frontend/package.json": `{"scripts":{"dev":"vite --host"}}`,
		"apps/api/package.json":      `{"scripts":{"dev":"next dev"}}`,
		"apps/ignored/package.json":  `{"scripts":{"dev":"next dev"}}`,
		"pnpm-workspace.yaml":        "packages:\n  - 'apps/*'\n  - '!apps/ignored'\n",
		"pnpm-lock.yaml":             "lockfileVersion: '9.0'\n",
	})
	loadedProject, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(loadedProject.Forwards, []Forward{{Name: "apps-api", Service: "web", Target: 3000}, {Name: "apps-frontend", Service: "web", Target: 5173}}) {
		t.Fatalf("unexpected workspace forwards %+v", loadedProject.Forwards)
	}
	service := decodeGenerated(t, loadedProject).Services["web"]
	if service.WorkingDir != "/workspace" ||
		!strings.Contains(service.Command[2], "cd /workspace && corepack enable pnpm && pnpm install --frozen-lockfile && exec pnpm run dev") {
		t.Fatalf("unexpected root service %+v", service)
	}
}

func TestExplicitConfigurationExamples(t *testing.T) {
	for _, mode := range []string{"compose", "commands", "node"} {
		t.Run(mode, func(t *testing.T) {
			loadedProject, err := Load(filepath.Join("..", "..", "examples", mode))
			if err != nil {
				t.Fatal(err)
			}
			if loadedProject.Mode != mode || loadedProject.Host != "devbox" || len(loadedProject.Forwards) == 0 || loadedProject.AutoPorts {
				t.Fatalf("unexpected %s example: %+v", mode, loadedProject)
			}
			if mode == "node" {
				service := decodeGenerated(t, loadedProject).Services["frontend"]
				if len(service.Volumes) != 1 || service.Volumes[0].Target != "/remote-source" {
					t.Fatalf("explicit Node install=none mounted dependencies: %+v", service.Volumes)
				}
			}
		})
	}
}

func TestCommandsEscapeComposeInterpolation(t *testing.T) {
	root := projectFixture(t, map[string]string{
		"remote-dev.toml": `host = "lab"
mode = "commands"
[services.web]
image = "alpine:3"
command = "echo $GREETING"
environment = { GREETING = "costs $20" }
[forwards.web]
service = "web"
target = 8000
local = 8123
`,
	})
	loadedProject, err := LoadWithHost(root, "override")
	if err != nil {
		t.Fatal(err)
	}
	service := decodeGenerated(t, loadedProject).Services["web"]
	if loadedProject.Host != "override" || *loadedProject.Forwards[0].Local != 8123 ||
		!slices.Equal(service.Command, []string{"/bin/sh", "-c", "echo $$GREETING"}) ||
		service.Environment["GREETING"] != "costs $$20" || service.Ports[0].HostIP != "127.0.0.1" {
		t.Fatalf("unexpected command project %+v or service %+v", loadedProject, service)
	}
}

func TestExplicitConfigurationValidation(t *testing.T) {
	cases := []struct{ config, message string }{
		{`host="-Fbad"` + "\n" + `mode="compose"`, "SSH host alias"},
		{`host="lab"` + "\n" + `mode="compose"` + "\n" + `compose_file="../compose.yaml"`, "project-relative"},
		{`host="lab"` + "\n" + `mode="compose"` + "\n" + `compose_file=".remote-dev.generated.json"`, "reserved"},
		{`host="lab"` + "\n" + `mode="compose"` + "\n" + `[forwards.web]` + "\n" + `service="web"` + "\n" + `target=true`, "target"},
		{`host="lab"` + "\n" + `mode="commands"` + "\n" + `[services.web]` + "\n" + `read_only=false`, "read_only"},
		{`host="lab"` + "\n" + `mode="commands"` + "\n" + `[services.web]` + "\n" + `[forwards.web]` + "\n" + `service="typo"` + "\n" + `target=8000`, "typo"},
	}
	for _, testCase := range cases {
		_, err := Load(projectFixture(t, map[string]string{"remote-dev.toml": testCase.config}))
		if err == nil || !strings.Contains(err.Error(), testCase.message) {
			t.Errorf("expected error containing %q for %s, got %v", testCase.message, testCase.config, err)
		}
	}
}

func TestInitDoesNotOverwriteConfiguration(t *testing.T) {
	for _, mode := range []string{"compose", "commands", "node"} {
		root := t.TempDir()
		filename, err := Init(root, "lab", mode)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Init(root, "another-host", mode); !errors.Is(err, os.ErrExist) {
			t.Fatalf("expected an existing config error, got %v", err)
		}
		content, err := os.ReadFile(filename)
		if err != nil || !strings.Contains(string(content), `host = "lab"`) || !strings.Contains(string(content), `mode = "`+mode+`"`) {
			t.Fatalf("unexpected init output %q, %v", content, err)
		}
	}
}

type generatedCompose struct {
	Services map[string]struct {
		Image       string
		WorkingDir  string `json:"working_dir"`
		Environment map[string]string
		Command     []string
		Labels      map[string]string
		Ports       []struct {
			HostIP string `json:"host_ip"`
			Target int
		}
		Volumes []struct {
			Source   string
			Target   string
			ReadOnly bool `json:"read_only"`
		}
	}
	Volumes map[string]any
}

func decodeGenerated(t *testing.T, loadedProject Project) generatedCompose {
	t.Helper()
	var generated generatedCompose
	if err := json.Unmarshal(loadedProject.Generated, &generated); err != nil {
		t.Fatalf("invalid generated Compose: %v", err)
	}
	return generated
}

func projectFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for filename, content := range files {
		filename = filepath.Join(root, filename)
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestDevelopmentOnlyComposeHasNoOverride(t *testing.T) {
	root := projectFixture(t, map[string]string{
		"docker-compose.dev.yml": "services:\n  web:\n    build: .\n",
	})
	loadedProject, err := LoadWithHost(root, "lab")
	if err != nil {
		t.Fatal(err)
	}
	if loadedProject.Host != "lab" || loadedProject.ComposeFile != "docker-compose.dev.yml" ||
		loadedProject.DetectedFrom != "docker-compose.dev.yml" || loadedProject.Override != "" {
		t.Fatalf("unexpected development-only project: %+v", loadedProject)
	}
}

func TestExplicitNodeInstallUsesNPMAndNamedDependencyVolume(t *testing.T) {
	root := projectFixture(t, map[string]string{
		"frontend/package.json":      `{"scripts":{"dev:web":"vite"}}`,
		"frontend/package-lock.json": "{}",
		"remote-dev.toml": `host = "lab"
mode = "node"
[services.frontend]
directory = "frontend"
script = "dev:web"
[forwards.web]
service = "frontend"
target = 3000
`,
	})
	loadedProject, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	generated := decodeGenerated(t, loadedProject)
	service := generated.Services["frontend"]
	if !slices.Equal(service.Command, []string{
		"/bin/sh", "-c", `cp -a /remote-source/. /workspace && npm ci && exec npm run -- "$$1"`, "remote-dev", "dev:web",
	}) || service.WorkingDir != "/workspace/frontend" || service.Environment["PORT"] != "3000" ||
		service.Environment["HOST"] != "0.0.0.0" || service.Labels["io.remote-dev.reload"] != "watch" {
		t.Fatalf("unexpected explicit Node service: %+v", service)
	}
	moduleVolume := service.Volumes[1]
	if _, found := generated.Volumes[moduleVolume.Source]; !found || moduleVolume.Target != "/workspace/frontend/node_modules" {
		t.Fatalf("unexpected explicit Node volume: %+v in %+v", moduleVolume, generated)
	}
}

func TestExplicitPathsCannotResolveOutsideTheProject(t *testing.T) {
	outside := projectFixture(t, map[string]string{"compose.yaml": "services: {}"})
	root := projectFixture(t, map[string]string{
		"remote-dev.toml": "host = \"lab\"\nmode = \"compose\"\ncompose_file = \"shared/compose.yaml\"\n",
	})
	if err := os.Symlink(outside, filepath.Join(root, "shared")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "must point inside the project") {
		t.Fatalf("expected project isolation error, got %v", err)
	}
}

func TestConfigurationMustBeARegularFile(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "remote-dev.toml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected a regular file error, got %v", err)
	}
}

func TestNPMWorkspaceUsesRootScriptWithOneApp(t *testing.T) {
	root := projectFixture(t, map[string]string{
		"package.json":          `{"scripts":{"dev":"npm run dev --workspaces"},"workspaces":["apps/*"]}`,
		"package-lock.json":     "{}",
		"apps/api/package.json": `{"scripts":{"dev":"nest start --watch"}}`,
	})
	loadedProject, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(loadedProject.Forwards, []Forward{{Name: "apps-api", Service: "web", Target: 3000}}) {
		t.Fatalf("unexpected workspace forwards: %+v", loadedProject.Forwards)
	}
	command := decodeGenerated(t, loadedProject).Services["web"].Command[2]
	if !strings.Contains(command, "cd /workspace && npm ci && exec npm run dev") {
		t.Fatalf("expected the workspace script to run at the root, got %q", command)
	}
}

func TestPNPMWorkspaceInstallsAtRootWhenAServiceNeedsFlags(t *testing.T) {
	root := projectFixture(t, map[string]string{
		"package.json":               `{"scripts":{"dev":"pnpm -r --parallel run dev"},"packageManager":"pnpm@10.11.0"}`,
		"apps/frontend/package.json": `{"scripts":{"dev":"vite"}}`,
		"pnpm-workspace.yaml":        "packages:\n  - 'apps/*'\n",
		"pnpm-lock.yaml":             "lockfileVersion: '9.0'\n",
	})
	loadedProject, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(loadedProject.Forwards, []Forward{{Name: "apps-frontend", Service: "apps-frontend", Target: 5173}}) {
		t.Fatalf("unexpected workspace forwards: %+v", loadedProject.Forwards)
	}
	service := decodeGenerated(t, loadedProject).Services["apps-frontend"]
	if service.WorkingDir != "/workspace/apps/frontend" || service.Volumes[1].Target != "/workspace/node_modules" {
		t.Fatalf("unexpected workspace service: %+v", service)
	}
	if !strings.Contains(service.Command[2], "cd /workspace && corepack enable pnpm && pnpm install --frozen-lockfile && cd /workspace/apps/frontend && exec pnpm run dev --host 0.0.0.0") {
		t.Fatalf("expected an app-specific command after workspace installation, got %v", service.Command)
	}
}

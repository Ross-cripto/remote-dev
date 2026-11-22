package project

import (
	"strings"
	"testing"
)

func TestLoadTestsPreservesDetectedComposeOverride(t *testing.T) {
	root := projectFixture(t, map[string]string{
		"compose.yaml":     "services:\n  app:\n    image: alpine:3\n",
		"compose.dev.yaml": "services:\n  tests:\n    image: alpine:3\n",
		"package.json":     `{"scripts":{"test":"node --test"}}`,
	})
	detected, service, err := LoadTests(root, "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	if detected.Host != "lab" || detected.ComposeFile != "compose.yaml" || detected.Override != "compose.dev.yaml" ||
		len(detected.Generated) != 0 || service != "" {
		t.Fatalf("expected the detected Compose files to determine the test service, got %+v and %q", detected, service)
	}
}

func TestLoadTestsUsesConfiguredComposeAndHostOverride(t *testing.T) {
	root := projectFixture(t, map[string]string{
		"remote-dev.toml": "host = \"config-lab\"\nmode = \"compose\"\ncompose_file = \"ci/compose.yaml\"\n",
		"ci/compose.yaml": "include: [tests.yaml]\n",
		"compose.yaml":    "services:\n  web:\n    image: alpine:3\n",
	})
	detected, service, err := LoadTests(root, "person@other-lab", "integration")
	if err != nil {
		t.Fatal(err)
	}
	if detected.ComposeFile != "ci/compose.yaml" || detected.Host != "person@other-lab" || service != "integration" {
		t.Fatalf("expected configured Compose file and CLI overrides, got %+v and %q", detected, service)
	}
}

func TestLoadTestsGeneratesIsolatedNodeService(t *testing.T) {
	root := projectFixture(t, map[string]string{
		"package.json":   `{"scripts":{"dev":"custom-launcher","test":"node --test"},"packageManager":"pnpm@10.11.0"}`,
		"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
		".node-version":  "22.14.0\n",
	})
	detected, service, err := LoadTests(root, "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	generated := decodeGenerated(t, detected)
	testService := generated.Services["test"]
	if detected.Mode != "node" || detected.Host != "lab" || detected.ComposeFile != GeneratedComposeFile ||
		detected.AutoPorts || len(detected.Forwards) != 0 || service != "test" || len(generated.Services) != 1 {
		t.Fatalf("expected an isolated Node test project, got %+v and %q", detected, service)
	}
	if testService.Image != "node:22-bookworm-slim" || testService.WorkingDir != "/workspace" ||
		len(testService.Labels) != 0 || len(testService.Ports) != 0 || testService.Environment["CI"] != "true" {
		t.Fatalf("unexpected test service settings: %+v", testService)
	}
	if len(testService.Command) != 3 || !strings.Contains(testService.Command[2], "pnpm install --frozen-lockfile && exec pnpm run test") {
		t.Fatalf("expected a one-off test command, got %v", testService.Command)
	}
}

func TestLoadTestsRejectsMissingTestScripts(t *testing.T) {
	for _, script := range []string{
		`{"scripts":{"dev":"vite"}}`,
		`{"scripts":{"test":"  "}}`,
		`{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`,
		`{"scripts":{"test":"echo 'Error: no test specified' && exit 1"}}`,
	} {
		root := projectFixture(t, map[string]string{
			"package.json":               script,
			"apps/frontend/package.json": `{"scripts":{"test":"vitest run"}}`,
		})
		if _, _, err := LoadTests(root, "", ""); err == nil || !strings.Contains(err.Error(), "test script") {
			t.Errorf("expected an actionable error for %s, got %v", script, err)
		}
	}
}

func TestLoadTestsRejectsUnknownServiceForDetectedNode(t *testing.T) {
	root := projectFixture(t, map[string]string{"package.json": `{"scripts":{"test":"node --test"}}`})
	if _, _, err := LoadTests(root, "", "integration"); err == nil || !strings.Contains(err.Error(), "integration") {
		t.Fatalf("expected unknown service error, got %v", err)
	}
}

func TestLoadTestsSelectsConfiguredCommand(t *testing.T) {
	root := projectFixture(t, map[string]string{
		"remote-dev.toml": `host = "configured-lab"
mode = "commands"
[services.web]
image = "alpine:3"
command = "run-dev"
[services.integration]
image = "alpine:3"
command = ["run-tests", "--integration"]
[forwards.integration]
service = "integration"
target = 8080
`,
	})
	detected, service, err := LoadTests(root, "", "integration")
	if err != nil {
		t.Fatal(err)
	}
	generated := decodeGenerated(t, detected)
	integration := generated.Services["integration"]
	if detected.Host != "configured-lab" || service != "integration" || len(generated.Services) != 1 ||
		len(detected.Forwards) != 0 || len(integration.Ports) != 0 ||
		len(integration.Command) != 2 || integration.Command[0] != "run-tests" {
		t.Fatalf("expected only the configured integration command, got %+v and %q", generated, service)
	}
}

func TestLoadTestsSelectsExplicitNodeScript(t *testing.T) {
	root := projectFixture(t, map[string]string{
		"remote-dev.toml": `host = "configured-lab"
mode = "node"
[services.tests]
directory = "app"
script = "test:unit"
install = "none"
`,
		"app/package.json": `{"scripts":{"test:unit":"node --test"}}`,
		"package.json":     `{"scripts":{"test":"vitest run"}}`,
	})
	detected, service, err := LoadTests(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	testService := decodeGenerated(t, detected).Services["tests"]
	if service != "tests" || testService.WorkingDir != "/workspace/app" || len(testService.Labels) != 0 ||
		len(testService.Command) != 5 || testService.Command[4] != "test:unit" {
		t.Fatalf("expected the configured Node test script, got %+v and %q", testService, service)
	}
}

func TestLoadTestsRejectsImplicitDevelopmentScriptInNodeConfig(t *testing.T) {
	root := projectFixture(t, map[string]string{
		"remote-dev.toml": `host = "configured-lab"
mode = "node"
[services.tests]
install = "none"
`,
		"package.json": `{"scripts":{"dev":"vite"}}`,
	})
	if _, _, err := LoadTests(root, "", ""); err == nil || !strings.Contains(err.Error(), "services.tests.script") {
		t.Fatalf("expected an explicit test script error, got %v", err)
	}
}

func TestLoadTestsUsesConfiguredHostForRootNodeScript(t *testing.T) {
	root := projectFixture(t, map[string]string{
		"remote-dev.toml": `host = "configured-lab"
mode = "node"
[services.web]
script = "dev"
`,
		"package.json": `{"scripts":{"dev":"vite","test":"node --test"}}`,
	})
	detected, service, err := LoadTests(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if generated := decodeGenerated(t, detected); len(generated.Services) != 1 || detected.Host != "configured-lab" || service != "test" {
		t.Fatalf("expected the root Node test script and configured host, got %+v and %q", detected, service)
	}
}

func TestLoadTestsRejectsAmbiguousConfiguredServices(t *testing.T) {
	root := projectFixture(t, map[string]string{
		"remote-dev.toml": `host = "configured-lab"
mode = "commands"
[services.test]
image = "alpine:3"
command = "unit-tests"
[services.tests]
image = "alpine:3"
command = "integration-tests"
`,
	})
	if _, _, err := LoadTests(root, "", ""); err == nil || !strings.Contains(err.Error(), "--service") {
		t.Fatalf("expected service selection error, got %v", err)
	}
}

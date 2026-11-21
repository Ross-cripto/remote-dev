package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureKey = "1234567890abcdef12345678"

func hostFixture(t *testing.T) (workspace, calls string) {
	t.Helper()
	directory := t.TempDir()
	t.Setenv("XDG_DATA_HOME", directory)
	t.Setenv("RD_TEST_ROOT", directory)
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COMPOSE_IGNORE_ORPHANS", "true")
	docker := `#!/bin/sh
set -eu
test -z "${COMPOSE_IGNORE_ORPHANS:-}"
printf '%s\n' "$*" >> "$RD_TEST_ROOT/calls"
case "$*" in
  'info --format {{.OSType}}') printf 'linux\n';;
  'compose version') printf 'Docker Compose version v2.39.0\n';;
  'ps --filter label=com.docker.compose.project=rd-'*)
    if [ -f "$RD_TEST_ROOT/running" ]; then printf 'abc123\tweb\n'; fi;;
  'ps --quiet --filter label=com.docker.compose.project=rd-'*)
    if [ -f "$RD_TEST_ROOT/running" ]; then printf 'abc123\n'; fi;;
  'ps --all --filter label=com.docker.compose.project=rd-'*)
    printf 'SERVICE   STATUS\nweb       Up\n';;
  'stop abc123') rm "$RD_TEST_ROOT/running";;
  'compose '*)
    case "$*" in
      *' config --profiles') printf 'production\ndev\n';;
      *' config --format json') cat "$RD_TEST_ROOT/compose-response";;
      *' up --detach --wait --wait-timeout 180'*)
        if [ -f "$RD_TEST_ROOT/fail-up" ]; then exit 1; fi
        touch "$RD_TEST_ROOT/running"
        printf 'Starting container\n';;
      *' port web 3000') printf '127.0.0.1:49152\n';;
      *' logs --no-color --tail 120') printf 'web  | hello\n';;
      *' restart web') printf 'Restarting container\n';;
      *) exit 65;;
    esac;;
  'cp - abc123:/workspace') cat > "$RD_TEST_ROOT/copied.tar";;
  'exec abc123 rm -f -- /workspace/'*) :;;
  *) exit 66;;
esac
`
	if err := os.WriteFile(filepath.Join(directory, "docker"), []byte(docker), 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(directory, "remote-dev", "standalone-v1", fixtureKey), filepath.Join(directory, "calls")
}

func hostFixtureWrite(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("RD_TEST_ROOT"), name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runFixture(t *testing.T, action, request string, payload []byte) (string, string, error) {
	t.Helper()
	var output, progress bytes.Buffer
	err := Run(context.Background(), []string{action, fixtureKey, request}, bytes.NewReader(payload), &output, &progress)
	return output.String(), progress.String(), err
}

func fixtureActivation(t *testing.T, action, digest string, delta bool, payload []byte) (string, string, error) {
	t.Helper()
	request := fmt.Sprintf(`{"digest":%q,"compose_file":"compose.yml","auto_ports":true,"delta":%t,"forwards":[]}`, digest, delta)
	return runFixture(t, action, request, payload)
}

func TestServerUsesWorkspaceProjectAndRewritesPorts(t *testing.T) {
	workspace, calls := hostFixture(t)
	hostFixtureWrite(t, "compose-response", `{"services":{"db":{"image":"postgres:16","ports":[{"target":5432}]},"web":{"image":"web","ports":[{"target":3000,"published":"9999","host_ip":"0.0.0.0"}],"command":["echo","$DEMO"]}}}`)
	base, archive := fixtureArchive(t, fixtureFile{"compose.yml", "services: {}", 0o644})
	output, progress, err := fixtureActivation(t, "up", base, false, archive)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, `"web":49152`) || !strings.Contains(progress, "Starting container") {
		t.Fatalf("up reported %q, progress %q", output, progress)
	}
	resolved, err := os.ReadFile(filepath.Join(workspace, "runtime-compose.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(resolved, &parsed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(resolved), "9999") || strings.Contains(string(resolved), "5432") || !strings.Contains(string(resolved), "127.0.0.1") || !strings.Contains(string(resolved), "$$DEMO") {
		t.Fatalf("incorrect runtime port bindings or interpolation: %s", resolved)
	}
	if strings.Contains(output, "Starting container") {
		t.Fatalf("Docker progress polluted the protocol: %q", output)
	}
	commands, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"--project-name rd-" + fixtureKey, "--profile dev", "--project-directory ", "--remove-orphans"} {
		if !strings.Contains(string(commands), part) {
			t.Fatalf("Compose invocation misses %q: %s", part, commands)
		}
	}
	for action, expected := range map[string]string{"status": "web", "logs": "hello", "stop": "Workspace stopped."} {
		output, _, err := runFixture(t, action, "{}", nil)
		if err != nil || !strings.Contains(output, expected) {
			t.Fatalf("%s returned %q, error %v", action, output, err)
		}
	}
}

func TestServerRetriesInterruptedActivationAndAppliesWatchDelta(t *testing.T) {
	workspace, calls := hostFixture(t)
	hostFixtureWrite(t, "compose-response", `{"services":{"web":{"image":"node:24","expose":[3000],"labels":{"io.remote-dev.live-source":"/workspace","io.remote-dev.reload":"watch"}}}}`)
	hostFixtureWrite(t, "fail-up", "")
	base, original := fixtureArchive(t, fixtureFile{"compose.yml", "services: {}", 0o644}, fixtureFile{"app.js", "old", 0o644}, fixtureFile{"old.js", "removed", 0o644})
	if _, _, err := fixtureActivation(t, "up", base, false, original); err == nil {
		t.Fatal("activation should report Docker failure")
	}
	if _, err := os.Stat(filepath.Join(workspace, "source-update.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(os.Getenv("RD_TEST_ROOT"), "fail-up")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixtureActivation(t, "up", base, false, original); err != nil {
		t.Fatal(err)
	}
	digest, delta := fixtureArchive(t,
		fixtureFile{".remote-dev.delta.json", fmt.Sprintf(`{"base":%q,"deleted":["old.js"]}`, base), 0o644},
		fixtureFile{"app.js", "after", 0o644},
	)
	if _, _, err := fixtureActivation(t, "sync", digest, true, delta); err != nil {
		t.Fatal(err)
	}
	requireFile(t, filepath.Join(workspace, "runtime-source", "app.js"), "after")
	if _, err := os.Stat(filepath.Join(workspace, "source-update.json")); !os.IsNotExist(err) {
		t.Fatalf("recovery journal remains after success: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "snapshots", base)); !os.IsNotExist(err) {
		t.Fatalf("outdated snapshot was retained: %v", err)
	}
	commands, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"--force-recreate", "exec abc123 rm -f -- /workspace/old.js", "cp - abc123:/workspace"} {
		if !strings.Contains(string(commands), part) {
			t.Fatalf("Compose invocation misses %q: %s", part, commands)
		}
	}
}

func TestServerRejectsUnsafeRuntimeAndWorkspaceIdentity(t *testing.T) {
	workspace, _ := hostFixture(t)
	hostFixtureWrite(t, "compose-response", `{"services":{"web":{"image":"web","network_mode":"host"}}}`)
	base, archive := fixtureArchive(t, fixtureFile{"compose.yml", "services: {}", 0o644})
	if _, _, err := fixtureActivation(t, "up", base, false, archive); err == nil || !strings.Contains(err.Error(), "project networking") {
		t.Fatalf("expected project networking error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "runtime-source")); !os.IsNotExist(err) {
		t.Fatalf("unsafe project created runtime files: %v", err)
	}
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"stop", "../../other", "{}"}, strings.NewReader(""), &output, &output); err == nil {
		t.Fatal("unsafe workspace identity was accepted")
	}
}

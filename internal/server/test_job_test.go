package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testJobFixture(t *testing.T, config string) (string, string) {
	t.Helper()
	workspace, calls := hostFixture(t)
	docker := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$RD_TEST_ROOT/calls"
case "$*" in
  *' config --format json'|*' config --format json test'|*' config --format json suite')
    cat "$RD_TEST_ROOT/compose-response";;
  *' run --build -T --interactive=false --name rd-test-'*)
    while [ "$1" != '--file' ]; do shift; done
    cp "$2" "$RD_TEST_ROOT/resolved-test"
    if [ -f "$RD_TEST_ROOT/fail-run" ]; then
      printf 'there is no compatible image\n' >&2
      exit 1
    fi
    touch "$RD_TEST_ROOT/test-running"
    printf 'test output\n'
    printf 'test diagnostic\n' >&2
    if [ -f "$RD_TEST_ROOT/wait-for-cancel" ]; then
      while true; do sleep 1; done
    fi
    exit 231;;
  'container inspect --format {{json .State}} rd-test-'*)
    test -f "$RD_TEST_ROOT/test-running"
    printf '{"Status":"exited","ExitCode":231,"Error":""}\n';;
  *' down --volumes --remove-orphans --timeout 5 --rmi local')
    rm -f "$RD_TEST_ROOT/test-running"
    if [ -f "$RD_TEST_ROOT/fail-cleanup" ]; then
      printf 'storage driver is unavailable\n' >&2
      exit 1
    fi;;
  *) exit 66;;
esac
`
	hostFixtureWrite(t, "docker", docker)
	hostFixtureWrite(t, "compose-response", config)
	return workspace, calls
}

type testJobResult struct {
	output   string
	progress string
	err      error
}

func startTestJob(t *testing.T, service string, progressWriter io.Writer, requestDigest string) (*io.PipeWriter, <-chan testJobResult) {
	t.Helper()
	digest, archive := fixtureArchive(t, fixtureFile{"compose.yml", "services: {}", 0o644})
	if requestDigest != "" {
		digest = requestDigest
	}
	request := fmt.Sprintf(`{"digest":%q,"compose_file":"compose.yml","test_service":%q,"test_bytes":%d}`, digest, service, len(archive))
	reader, writer := io.Pipe()
	t.Cleanup(func() { writer.Close() })
	result := make(chan testJobResult, 1)
	go func() {
		defer reader.Close()
		var output, progress bytes.Buffer
		writer := io.Writer(&progress)
		if progressWriter != nil {
			writer = io.MultiWriter(progressWriter, &progress)
		}
		err := Run(context.Background(), []string{"test", fixtureKey, request}, reader, &output, writer)
		result <- testJobResult{output.String(), progress.String(), err}
	}()
	_, _ = writer.Write(archive)
	return writer, result
}

func waitTestJob(t *testing.T, result <-chan testJobResult) testJobResult {
	t.Helper()
	select {
	case reply := <-result:
		return reply
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not finish")
		return testJobResult{}
	}
}

func requireTestJobRemoved(t *testing.T) {
	t.Helper()
	root := os.Getenv("RD_TEST_ROOT")
	for _, filename := range []string{filepath.Join(root, "remote-dev", "test-jobs-v1", fixtureKey), filepath.Join(root, "test-running")} {
		if _, err := os.Stat(filename); !os.IsNotExist(err) {
			t.Fatalf("temporary test resource remains at %s: %v", filename, err)
		}
	}
}

func TestServerRunsTestWithEphemeralProject(t *testing.T) {
	workspace, calls := testJobFixture(t, `{
		"name":"rd-test-1234567890abcdef12345678",
		"services":{
			"test":{"image":"dev-app:latest","build":{"context":"."},"profiles":["test"],"depends_on":{"db":{"condition":"service_healthy"}},"command":["sh","-c","$DEMO"]},
			"db":{"image":"postgres:16","ports":[{"target":5432,"published":"5432"}]}
		},
		"networks":{"default":{"name":"rd-test-1234567890abcdef12345678_default"}},
		"volumes":{"test-db":{"name":"rd-test-1234567890abcdef12345678_test-db"}}
	}`)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspace, "existing")
	if err := os.WriteFile(marker, []byte("run state"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, results := startTestJob(t, "", nil, "")
	result := waitTestJob(t, results)
	if result.err != nil || result.output != "{\"exit_code\":231}\n" {
		t.Fatalf("test replied %q with error %v and progress %q", result.output, result.err, result.progress)
	}
	for _, message := range []string{"test output", "test diagnostic"} {
		if !strings.Contains(result.progress, message) {
			t.Fatalf("test progress is missing %q: %q", message, result.progress)
		}
	}
	commands, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"--project-name rd-test-" + fixtureKey, "--profile * config --format json", "config --format json test", "rd-test-" + fixtureKey + "-runner test", "down --volumes --remove-orphans"} {
		if !strings.Contains(string(commands), expected) {
			t.Fatalf("test commands missed %q: %s", expected, commands)
		}
	}
	resolved, err := os.ReadFile(filepath.Join(os.Getenv("RD_TEST_ROOT"), "resolved-test"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"ports"`, `"profiles"`, "dev-app:latest"} {
		if strings.Contains(string(resolved), forbidden) {
			t.Fatalf("isolated configuration kept %s: %s", forbidden, resolved)
		}
	}
	if !strings.Contains(string(resolved), "service_healthy") || !strings.Contains(string(resolved), "$$DEMO") {
		t.Fatalf("isolated configuration lost test semantics: %s", resolved)
	}
	requireFile(t, marker, "run state")
	requireTestJobRemoved(t)
}

func TestServerRejectsAmbiguousTestService(t *testing.T) {
	testJobFixture(t, `{"services":{"test":{"image":"alpine"},"tests":{"image":"alpine"}}}`)
	_, results := startTestJob(t, "", nil, "")
	result := waitTestJob(t, results)
	if result.err == nil || !strings.Contains(result.err.Error(), "--service") || result.output != "" {
		t.Fatalf("ambiguous test replied %#v", result)
	}
	requireTestJobRemoved(t)
}

func TestServerRejectsTestResourcesOutsideJob(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		config  string
		message string
	}{
		{"external network", `{"services":{"test":{"image":"alpine"}},"networks":{"default":{"external":true,"name":"shared"}}}`, "network"},
		{"explicit volume name", `{"services":{"test":{"image":"alpine"}},"volumes":{"database":{"name":"shared"}}}`, "volume"},
		{"named container", `{"services":{"test":{"image":"alpine","container_name":"shared"}}}`, "container name"},
		{"host network", `{"services":{"test":{"image":"alpine","network_mode":"host"}}}`, "project networking"},
		{"container namespace", `{"services":{"test":{"image":"alpine","pid":"container:shared"}}}`, "pid"},
		{"host path", `{"services":{"test":{"image":"alpine","volumes":[{"type":"bind","source":"/var/run/docker.sock","target":"/var/run/docker.sock"}]}}}`, "bind"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, calls := testJobFixture(t, testCase.config)
			_, results := startTestJob(t, "", nil, "")
			result := waitTestJob(t, results)
			if result.err == nil || !strings.Contains(result.err.Error(), testCase.message) || result.output != "" {
				t.Fatalf("unsafe test configuration replied %#v", result)
			}
			commands, err := os.ReadFile(calls)
			if err != nil || strings.Contains(string(commands), " run ") {
				t.Fatalf("unsafe test reached Docker run: %s, error %v", commands, err)
			}
			requireTestJobRemoved(t)
		})
	}
}

type testJobProgress chan struct{}

func (progress testJobProgress) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte("test output")) {
		select {
		case progress <- struct{}{}:
		default:
		}
	}
	return len(data), nil
}

func TestServerCancelsTestAndCleansUpWhenInputCloses(t *testing.T) {
	_, calls := testJobFixture(t, `{"services":{"suite":{"image":"alpine"}}}`)
	hostFixtureWrite(t, "wait-for-cancel", "")
	progress := make(testJobProgress, 1)
	input, results := startTestJob(t, "suite", progress, "")
	waitForTestProgress(t, progress, results)
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	result := waitTestJob(t, results)
	if result.err != nil || result.output != "{\"exit_code\":130}\n" {
		t.Fatalf("cancelled test replied %#v", result)
	}
	commands, err := os.ReadFile(calls)
	if err != nil || !strings.Contains(string(commands), "down --volumes --remove-orphans") {
		t.Fatalf("cancellation skipped cleanup: %s, error %v", commands, err)
	}
	requireTestJobRemoved(t)
}

func TestServerReportsCleanupFailureAfterTestCancellation(t *testing.T) {
	testJobFixture(t, `{"services":{"suite":{"image":"alpine"}}}`)
	hostFixtureWrite(t, "fail-cleanup", "")
	hostFixtureWrite(t, "wait-for-cancel", "")
	progress := make(testJobProgress, 1)
	input, results := startTestJob(t, "suite", progress, "")
	waitForTestProgress(t, progress, results)
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	result := waitTestJob(t, results)
	if result.err == nil || !strings.Contains(result.err.Error(), "storage driver is unavailable") || result.output != "" {
		t.Fatalf("failed cancellation cleanup replied %#v", result)
	}
	requireTestJobRemoved(t)
}

func TestServerRejectsExtraTestStreamData(t *testing.T) {
	testJobFixture(t, `{"services":{"suite":{"image":"alpine"}}}`)
	hostFixtureWrite(t, "wait-for-cancel", "")
	progress := make(testJobProgress, 1)
	input, results := startTestJob(t, "suite", progress, "")
	waitForTestProgress(t, progress, results)
	if _, err := input.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	result := waitTestJob(t, results)
	if result.err == nil || !strings.Contains(result.err.Error(), "unexpected data after test source") || result.output != "" {
		t.Fatalf("test with extra data replied %#v", result)
	}
	requireTestJobRemoved(t)
}

func waitForTestProgress(t *testing.T, progress testJobProgress, results <-chan testJobResult) {
	t.Helper()
	select {
	case <-progress:
	case result := <-results:
		t.Fatalf("test stopped before writing progress: %#v", result)
	case <-time.After(5 * time.Second):
		t.Fatal("test output did not stream before completion")
	}
}

func TestServerReportsTestJobInfrastructureErrors(t *testing.T) {
	for _, testCase := range []struct {
		trigger string
		message string
	}{
		{"fail-run", "test container"},
		{"fail-cleanup", "storage driver is unavailable"},
	} {
		t.Run(testCase.trigger, func(t *testing.T) {
			testJobFixture(t, `{"services":{"test":{"image":"alpine"}}}`)
			hostFixtureWrite(t, testCase.trigger, "")
			_, results := startTestJob(t, "", nil, "")
			result := waitTestJob(t, results)
			if result.err == nil || !strings.Contains(result.err.Error(), testCase.message) || result.output != "" {
				t.Fatalf("infrastructure error replied %#v", result)
			}
			requireTestJobRemoved(t)
		})
	}
}

func TestServerRejectsCorruptTestSource(t *testing.T) {
	_, calls := testJobFixture(t, `{"services":{"test":{"image":"alpine"}}}`)
	_, results := startTestJob(t, "", nil, strings.Repeat("0", 64))
	result := waitTestJob(t, results)
	if result.err == nil || !strings.Contains(result.err.Error(), "digest check") || result.output != "" {
		t.Fatalf("corrupt source replied %#v", result)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("invalid snapshot reached Docker: %v", err)
	}
	requireTestJobRemoved(t)
}

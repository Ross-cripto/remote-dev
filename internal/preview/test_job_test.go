package preview

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteTestResultPreservesExitCode(t *testing.T) {
	if err := parseTestResult([]byte(`{"exit_code":0}`)); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"23", "255"} {
		response := `{"exit_code":` + code + `}`
		var result *TestExitError
		if err := parseTestResult([]byte(response)); !errors.As(err, &result) || result.Error() != "tests exited with code "+code {
			t.Fatalf("unexpected result for %s: %v", response, err)
		}
	}
	for _, response := range []string{`{}`, `{"exit_code":null}`, `{"exit_code":-1}`, `{"exit_code":256}`, `bad JSON`} {
		var result *TestExitError
		err := parseTestResult([]byte(response))
		if err == nil || errors.As(err, &result) {
			t.Fatalf("expected a protocol error for %s, got %v", response, err)
		}
	}
}

func TestRemoteTestUploadStaysOpenUntilCanceled(t *testing.T) {
	sshDirectory := t.TempDir()
	mockSSH := `#!/bin/sh
archive=$(/bin/dd bs=1 count=8 2>/dev/null)
[ "$archive" = snapshot ] || exit 2
printf 'running tests\n' >&2
IFS= read -r unexpected && exit 3
printf 'cleaned up\n' >&2
printf '{"exit_code":130}\n'
`
	if err := os.WriteFile(filepath.Join(sshDirectory, "ssh"), []byte(mockSSH), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", sshDirectory)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var output bytes.Buffer
	logs := testLogWriter(func(data []byte) (int, error) {
		if strings.Contains(string(data), "running tests") {
			cancel()
		}
		return output.Write(data)
	})
	connection := Connection{Target: "devbox"}
	_, err := connection.streamTest(ctx, logs, strings.NewReader("snapshot"), "run-test")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected result: %v", err)
	}
	if output.String() != "running tests\ncleaned up\n" {
		t.Fatalf("expected logs through cancellation, got %q", output.String())
	}
}

type testLogWriter func([]byte) (int, error)

func (writer testLogWriter) Write(data []byte) (int, error) {
	return writer(data)
}

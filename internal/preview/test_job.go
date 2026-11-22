package preview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Ross-cripto/remote-dev/internal/payload"
	"github.com/Ross-cripto/remote-dev/internal/project"
)

const (
	testCleanupWait = 35 * time.Second
	maxTestExitCode = 255
	interruptedTest = 130
)

type TestExitError struct {
	Code int
}

func (result *TestExitError) Error() string {
	return fmt.Sprintf("tests exited with code %d", result.Code)
}

func parseTestResult(output []byte) error {
	var result struct {
		ExitCode *int `json:"exit_code"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return fmt.Errorf("read result from Linux test runner: %w", err)
	}
	if result.ExitCode == nil || *result.ExitCode < 0 || *result.ExitCode > maxTestExitCode {
		return errors.New("Linux test runner returned an invalid exit code")
	}
	if *result.ExitCode != 0 {
		return &TestExitError{Code: *result.ExitCode}
	}
	return nil
}

func (connection Connection) streamTest(ctx context.Context, writer io.Writer, archive io.Reader, remoteCommand string) ([]byte, error) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	transfer := make(chan error, 1)
	go func() {
		_, err := io.Copy(writeEnd, archive)
		if err != nil {
			writeEnd.Close()
		}
		transfer <- err
	}()
	command := exec.CommandContext(ctx, "ssh", connection.commandOptions(remoteCommand, false)...)
	command.Stdin = readEnd
	command.Stderr = writer
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = testCleanupWait
	command.Cancel = func() error {
		err := writeEnd.Close()
		if errors.Is(err, os.ErrClosed) {
			return nil
		}
		return err
	}
	output, runErr := command.Output()
	readEnd.Close()
	writeEnd.Close()
	transferErr := <-transfer
	if runErr != nil {
		return nil, runErr
	}
	if transferErr != nil {
		return nil, fmt.Errorf("send source snapshot: %w", transferErr)
	}
	return output, nil
}

func (client Client) testConnection(root, projectHost string, override bool) (Connection, error) {
	if !override {
		workspace, err := client.readWorkspace(root)
		if err == nil {
			return workspace.Connection, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return Connection{}, err
		}
	}
	return client.resolveConnection(projectHost)
}

func (client Client) runTests(ctx context.Context, writer io.Writer, root, host, service string) error {
	detected, testService, err := project.LoadTests(root, host, service)
	if err != nil {
		return err
	}
	connection, err := client.testConnection(root, detected.Host, host != "")
	if err != nil {
		return err
	}
	prepareContext, cancel := context.WithTimeout(ctx, remoteTimeout)
	ready, err := client.readyConnection(prepareContext, connection)
	cancel()
	if err != nil {
		return fmt.Errorf("prepare Linux test runner: %w", err)
	}
	snapshot, err := makeProjectSnapshot(ctx, root, nil, "", detected.Generated)
	if err != nil {
		return fmt.Errorf("capture test source: %w", err)
	}
	defer snapshot.Close()

	request := runtimeRequest(detected)
	request.TestService = testService
	return runTestSnapshot(ctx, writer, root, ready, snapshot, request)
}

func runTestSnapshot(ctx context.Context, writer io.Writer, root string, connection Connection, snapshot *Snapshot, request hostRequest) error {
	jobKey, err := randomID()
	if err != nil {
		return err
	}
	digest, err := payload.Digest(connection.Architecture)
	if err != nil {
		return err
	}
	request.Digest = snapshot.Digest
	request.TestBytes = snapshot.Bytes
	encodedRequest, err := json.Marshal(request)
	if err != nil {
		return err
	}
	fmt.Fprintf(writer, "Running tests on Linux for %s.\n", filepath.Base(root))
	command := helperCommand(digest, "test", jobKey, string(encodedRequest))
	output, err := connection.streamTest(ctx, writer, snapshot.File, command)
	if err != nil {
		if ctx.Err() != nil {
			if !errors.Is(err, ctx.Err()) {
				fmt.Fprintf(writer, "Unable to confirm remote cleanup: %v\n", err)
			}
			return &TestExitError{Code: interruptedTest}
		}
		return fmt.Errorf("run tests on Linux: %w", err)
	}
	return parseTestResult(output)
}

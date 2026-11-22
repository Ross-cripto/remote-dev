package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

const (
	testCleanupTimeout      = 30 * time.Second
	testStopSeconds         = "5"
	dockerPipeTimeout       = 2 * time.Second
	maxTestExitCode         = 255
	interruptedTestExitCode = 130
)

var errTestStreamClosed = errors.New("test stream closed")

type testReply struct {
	ExitCode int `json:"exit_code"`
}

type testContainerState struct {
	Status   string
	ExitCode *int
	Error    string
}

func (host hostCommand) runTest(key string, request hostRequest, input io.Reader) (result testReply, jobErr error) {
	if err := validateTestRequest(request, input); err != nil {
		return testReply{}, err
	}
	jobDirectory, err := createTestJobDirectory(key)
	if err != nil {
		return testReply{}, err
	}
	defer func() {
		if err := os.RemoveAll(jobDirectory); err != nil {
			jobErr = errors.Join(jobErr, fmt.Errorf("remove temporary test directory: %w", err))
		}
	}()

	payload := &io.LimitedReader{R: input, N: request.TestBytes}
	source, err := acceptSnapshot(jobDirectory, request, "", payload)
	if err != nil {
		return testReply{}, fmt.Errorf("receive test source: %w", err)
	}
	if payload.N != 0 {
		return testReply{}, errors.New("test source ended before the declared size")
	}

	jobContext, cancel := context.WithCancelCause(host.context)
	defer cancel(nil)
	go watchTestStream(input, cancel)
	host.context = jobContext
	host.ownProcessGroup = true
	result, err = host.runComposeTest(jobDirectory, "rd-test-"+key, source, request)
	switch cause := context.Cause(jobContext); {
	case cause == nil:
		return result, err
	case errors.Is(cause, errTestStreamClosed), errors.Is(cause, context.Canceled):
		if err != nil {
			return testReply{}, err
		}
		return testReply{ExitCode: interruptedTestExitCode}, nil
	default:
		return testReply{}, errors.Join(cause, err)
	}
}

func watchTestStream(input io.Reader, cancel context.CancelCauseFunc) {
	var trailing [1]byte
	_, err := io.ReadFull(input, trailing[:])
	switch {
	case errors.Is(err, io.EOF):
		cancel(errTestStreamClosed)
	case err != nil:
		cancel(fmt.Errorf("read test stream: %w", err))
	default:
		cancel(errors.New("unexpected data after test source"))
	}
}

func validateTestRequest(request hostRequest, input io.Reader) error {
	if err := validateRequest(request); err != nil {
		return err
	}
	if request.Delta || request.TestBytes <= 0 || input == nil {
		return errors.New("test requires a full snapshot with a declared size")
	}
	if request.TestService != "" && !forwardName.MatchString(request.TestService) {
		return fmt.Errorf("invalid test service name %q", request.TestService)
	}
	return nil
}

func createTestJobDirectory(key string) (string, error) {
	root, err := dataDirectory()
	if err != nil {
		return "", err
	}
	directory, err := workspacePath(root, "remote-dev/test-jobs-v1/"+key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(directory), 0o700); err != nil {
		return "", err
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return "", fmt.Errorf("create temporary test directory: %w", err)
	}
	return directory, nil
}

func (host hostCommand) runComposeTest(jobDirectory, project, source string, request hostRequest) (result testReply, jobErr error) {
	config, service, err := host.resolveTestConfig(project, source, request)
	if err != nil {
		return testReply{}, ignoreTestCancellation(err)
	}
	if err := config.isolateTest(project, source); err != nil {
		return testReply{}, err
	}
	if _, err := saveRuntimeConfig(jobDirectory, config); err != nil {
		return testReply{}, fmt.Errorf("save isolated test configuration: %w", err)
	}
	command := append(runtimeComposeCommand(jobDirectory, project, source), "--env-file", os.DevNull)
	defer func() {
		if err := host.cleanTestProject(command, source); err != nil {
			jobErr = errors.Join(jobErr, err)
		}
	}()
	result, err = host.runTestContainer(command, source, project+"-runner", service)
	return result, ignoreTestCancellation(err)
}

func ignoreTestCancellation(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (host hostCommand) resolveTestConfig(project, source string, request hostRequest) (composeSpec, string, error) {
	request.AutoPorts = false
	request.ComposeProfiles = []string{"*"}
	service := request.TestService
	if service == "" {
		all, err := host.composeConfig(project, source, request)
		if err != nil {
			return composeSpec{}, "", err
		}
		service, err = defaultTestService(all)
		if err != nil {
			return composeSpec{}, "", err
		}
	}
	config, err := host.composeConfig(project, source, request, service)
	if err != nil {
		return composeSpec{}, "", fmt.Errorf("resolve test service %q: %w", service, err)
	}
	if _, exists := config.services[service]; !exists {
		return composeSpec{}, "", fmt.Errorf("no Compose service named %q", service)
	}
	return config, service, nil
}

func defaultTestService(config composeSpec) (string, error) {
	_, hasTest := config.services["test"]
	_, hasTests := config.services["tests"]
	switch {
	case hasTest && hasTests:
		return "", errors.New("both Compose services test and tests exist. Choose one with --service")
	case hasTest:
		return "test", nil
	case hasTests:
		return "tests", nil
	default:
		return "", errors.New("no Compose service named test or tests. Choose a service with --service")
	}
}

func (host hostCommand) runTestContainer(command []string, source, name, service string) (testReply, error) {
	arguments := append(slices.Clone(command), "run", "--build", "-T", "--interactive=false", "--name", name, service)
	process := host.dockerCommand(source, arguments...)
	process.Stdout, process.Stderr = host.progress, host.progress
	runErr := process.Run()
	if err := host.context.Err(); err != nil {
		return testReply{}, err
	}

	output, err := host.docker("", "container", "inspect", "--format", "{{json .State}}", name)
	if err != nil {
		return testReply{}, errors.Join(fmt.Errorf("inspect test container: %w", err), runErr)
	}
	var state testContainerState
	if err := decodeJSON(bytes.NewReader(output), &state); err != nil {
		return testReply{}, fmt.Errorf("read test container state: %w", err)
	}
	if state.Error != "" {
		return testReply{}, fmt.Errorf("test container failed: %s", state.Error)
	}
	if state.Status != "exited" || state.ExitCode == nil {
		return testReply{}, errors.Join(fmt.Errorf("test container did not exit (status %q)", state.Status), runErr)
	}
	return testExitReply(*state.ExitCode, runErr)
}

func testExitReply(status int, runErr error) (testReply, error) {
	if status < 0 || status > maxTestExitCode {
		return testReply{}, fmt.Errorf("invalid test container exit status %d", status)
	}
	if runErr != nil {
		var exited *exec.ExitError
		if !errors.As(runErr, &exited) || exited.ExitCode() != status {
			return testReply{}, fmt.Errorf("docker compose run failed: %w", runErr)
		}
	}
	return testReply{ExitCode: status}, nil
}

func (host hostCommand) cleanTestProject(command []string, source string) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(host.context), testCleanupTimeout)
	defer cancel()
	host.context = cleanup
	arguments := append(slices.Clone(command), "down", "--volumes", "--remove-orphans", "--timeout", testStopSeconds, "--rmi", "local")
	if _, err := host.docker(source, arguments...); err != nil {
		return fmt.Errorf("clean isolated test project: %w", err)
	}
	return nil
}

func isolateDockerProcess(command *exec.Cmd) {
	// Docker runs Compose in a CLI plugin process that can outlive the CLI itself.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = dockerPipeTimeout
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

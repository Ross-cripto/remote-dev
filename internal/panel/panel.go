package panel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const (
	enterScreen   = "\x1b[?1049h\x1b[?25l"
	leaveScreen   = "\x1b[0m\x1b[?25h\x1b[?1049l"
	pollMillis    = 40
	keyBufferSize = 128
)

type Workspace struct {
	Root    string
	Name    string
	Host    string
	Mode    string
	Current bool
}

type command struct {
	action    string
	label     string
	workspace Workspace
}

type jobResult struct {
	command    command
	workspaces []Workspace
	output     string
	err        error
}

func IsTerminal(input, output *os.File) bool {
	return input != nil && output != nil && term.IsTerminal(int(input.Fd())) && term.IsTerminal(int(output.Fd()))
}

func Run(
	ctx context.Context,
	input, output *os.File,
	discover func(context.Context) ([]Workspace, error),
	invoke func(context.Context, string, Workspace) (string, error),
) (runErr error) {
	if !IsTerminal(input, output) {
		return errors.New("the Remote Dev panel needs an interactive terminal")
	}
	inputFD, outputFD := int(input.Fd()), int(output.Fd())
	original, err := term.MakeRaw(inputFD)
	if err != nil {
		return fmt.Errorf("configure terminal: %w", err)
	}
	defer func() {
		_, screenErr := io.WriteString(output, leaveScreen)
		runErr = errors.Join(runErr, screenErr, term.Restore(inputFD, original))
	}()
	if _, err := io.WriteString(output, enterScreen); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan jobResult, 1)
	start := func(command command) {
		go func() {
			result := jobResult{command: command}
			if command.action == "refresh" {
				result.workspaces, result.err = discover(ctx)
			} else {
				result.output, result.err = invoke(ctx, command.action, command.workspace)
			}
			results <- result
		}()
	}

	state := panelState{}
	start(state.refresh())
	palette := terminalPalette()
	var previousSize terminalSize
	dirty := true
	var decoder keyDecoder
	for ctx.Err() == nil {
		select {
		case result := <-results:
			state.finish(result)
			dirty = true
		default:
		}
		width, height, err := term.GetSize(outputFD)
		if err != nil {
			return fmt.Errorf("read terminal size: %w", err)
		}
		size := terminalSize{width: width, height: height}
		if dirty || size != previousSize {
			if _, err := io.WriteString(output, drawPanel(&state, size).render(palette)); err != nil {
				return err
			}
			previousSize, dirty = size, false
		}

		keys, err := pollKeys(inputFD, &decoder)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read terminal: %w", err)
		}
		for _, pressed := range keys {
			next, exit := state.press(pressed)
			if exit {
				return nil
			}
			if next.action != "" {
				start(next)
			}
			dirty = true
		}
	}
	return nil
}

func pollKeys(inputFD int, decoder *keyDecoder) ([]key, error) {
	poll := []unix.PollFd{{Fd: int32(inputFD), Events: unix.POLLIN}}
	ready, err := unix.Poll(poll, pollMillis)
	if errors.Is(err, unix.EINTR) {
		return nil, nil
	}
	if err != nil || ready == 0 {
		return nil, err
	}
	if poll[0].Revents&unix.POLLIN == 0 {
		return nil, io.EOF
	}
	var input [keyBufferSize]byte
	count, err := unix.Read(inputFD, input[:])
	if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, io.EOF
	}
	return decoder.feed(input[:count]), nil
}

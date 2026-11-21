package panel

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

var fixtureWorkspaces = []Workspace{
	{Root: "/work/demo", Name: "Demo", Host: "devbox", Mode: "Compose", Current: true},
	{Root: "/work/topic", Name: "Topic", Host: "second-box", Mode: "Compose"},
}

func stateWithWorkspaces() panelState {
	return panelState{workspaces: fixtureWorkspaces}
}

func TestPanelRoutesShortcutsToSelectedWorkspace(t *testing.T) {
	shortcuts := []struct {
		key  key
		want string
	}{
		{'s', "start"},
		{'x', "stop"},
		{'i', "status"},
		{'l', "logs"},
		{'u', "sync"},
		{'o', "open"},
	}
	for _, shortcut := range shortcuts {
		t.Run(shortcut.want, func(t *testing.T) {
			state := stateWithWorkspaces()
			state.press(keyDown)
			command, exit := state.press(shortcut.key)
			if exit || command.action != shortcut.want || command.workspace.Root != "/work/topic" {
				t.Fatalf("press(%q) returned %+v, exit %v", shortcut.key, command, exit)
			}
		})
	}
}

func TestPanelPreservesPendingActionDuringNavigation(t *testing.T) {
	state := stateWithWorkspaces()
	state.press(keyDown)
	command, _ := state.press('s')
	state.press(keyUp)

	if state.selected().Root != "/work/demo" {
		t.Fatalf("arrow keys did not change selection during an action: %+v", state.selected())
	}
	if next, _ := state.press('x'); next.action != "" {
		t.Fatalf("started a second action while the first was pending: %+v", next)
	}
	if _, exit := state.press('q'); exit || !strings.Contains(state.message, "Topic") {
		t.Fatalf("quit with work pending or lost the pending target: %v %q", exit, state.message)
	}
	state.finish(jobResult{command: command, output: "Workspace running."})
	if !strings.Contains(state.message, "Topic") || state.output != "Workspace running." {
		t.Fatalf("completed action lost its target or output: %+v", state)
	}
	if _, exit := state.press('q'); !exit {
		t.Fatal("quit blocked after action finished")
	}
}

func TestRefreshPreservesSelectionByRoot(t *testing.T) {
	state := stateWithWorkspaces()
	state.press(keyDown)
	command, _ := state.press('r')
	state.finish(jobResult{command: command, workspaces: []Workspace{
		fixtureWorkspaces[1], fixtureWorkspaces[0],
	}})
	if state.selected().Root != "/work/topic" {
		t.Fatalf("selection moved after refresh: %+v", state.selected())
	}
}

func TestFailurePreservesCommandOutput(t *testing.T) {
	state := stateWithWorkspaces()
	command, _ := state.press('u')
	state.finish(jobResult{command: command, output: "Transfer started", err: errors.New("SSH connection failed")})
	if !state.failed || !strings.Contains(state.output, "Transfer started") || !strings.Contains(state.output, "SSH connection failed") {
		t.Fatalf("failure did not preserve output and error: %+v", state)
	}
	if next, _ := state.press('i'); next.action != "status" {
		t.Fatalf("action unavailable after error: %+v", next)
	}
}

func TestKeyboardDecodesSplitEscapeSequences(t *testing.T) {
	decoder := keyDecoder{}
	var got []key
	for _, chunk := range []string{"\x1b[", "B\x1bO", "A\x1b[5", "~\x1b[6~s\x03"} {
		got = append(got, decoder.feed([]byte(chunk))...)
	}
	want := []key{keyDown, keyUp, keyPageUp, keyPageDown, 's', keyInterrupt}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded %v, want %v", got, want)
	}
}

func TestFrameFitsNarrowAndWideTerminals(t *testing.T) {
	for _, size := range []terminalSize{{width: 120, height: 26}, {width: 44, height: 20}, {width: 1, height: 1}} {
		state := stateWithWorkspaces()
		state.output = "serve http://127.0.0.1:8000"
		frame := drawPanel(&state, size)
		for _, span := range frame.spans {
			if span.row >= size.height || span.column+cellWidth(span.text) > size.width {
				t.Fatalf("span %+v extends beyond terminal %+v", span, size)
			}
		}
		rendered := frame.render(palette{})
		if size.width > 1 {
			for _, want := range []string{"Remote Dev", "devbox", "serve http://127.0.0.1:8000", "[q] Quit"} {
				if !strings.Contains(rendered, want) {
					t.Fatalf("terminal %+v did not render %q: %q", size, want, rendered)
				}
			}
		}
	}
}

func TestActivityPagesThroughSanitizedOutput(t *testing.T) {
	state := stateWithWorkspaces()
	state.output = "\x1b[31mFirst line\x1b[0m\n" + strings.Repeat("Service is ready\n", 20) + "\x1b]0;unsafe title\x07Last line"
	size := terminalSize{width: 44, height: 20}
	initial := drawPanel(&state, size).render(palette{})
	if !strings.Contains(initial, "First line") || strings.Contains(initial, "Last line") || strings.Contains(initial, "\x1b[31m") {
		t.Fatalf("bad first output page: %q", initial)
	}
	for range 20 {
		state.press(keyPageDown)
	}
	last := drawPanel(&state, size).render(palette{})
	if !strings.Contains(last, "Last line") || strings.Contains(last, "unsafe title") {
		t.Fatalf("bad last output page: %q", last)
	}
	state.press(keyPageUp)
	previous := drawPanel(&state, size).render(palette{})
	if strings.Contains(previous, "Last line") {
		t.Fatalf("page up did not leave the last page: %q", previous)
	}
}

func TestOutputWrapsWideCharactersAndKeepsCombiningMarks(t *testing.T) {
	lines := wrapCells("ab雪e\u0301\tX", 4)
	want := []string{"ab雪", "e\u0301   ", "X"}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("wrapped %q, want %q", lines, want)
	}
}

func TestRunRequiresAnInteractiveTerminal(t *testing.T) {
	input, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer output.Close()
	err = Run(context.Background(), input, output, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("Run with a pipe returned %v", err)
	}
}

func TestInterruptExitsEvenAfterAnEscapePrefix(t *testing.T) {
	decoder := keyDecoder{}
	decoder.feed([]byte("\x1b["))
	got := decoder.feed([]byte("\x03"))
	if !reflect.DeepEqual(got, []key{keyInterrupt}) {
		t.Fatalf("interrupt was swallowed by pending escape sequence: %v", got)
	}
}

func TestCompactFramePreservesAvailableShortcuts(t *testing.T) {
	state := stateWithWorkspaces()
	state.output = "Service logs are ready"
	rendered := drawPanel(&state, terminalSize{width: 60, height: 10}).render(palette{})
	for _, want := range []string{"Demo", "Service logs are ready", "[i] Status", "[q] Quit"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("compact frame did not render %q: %q", want, rendered)
		}
	}
}

func TestNarrowFramePrioritizesStartupOutput(t *testing.T) {
	state := stateWithWorkspaces()
	state.workspaces = append(append([]Workspace{}, fixtureWorkspaces...), Workspace{Root: "/work/third", Name: "Third"})
	state.output = "Workspace running.\nDetected Compose: compose.yaml\nweb  http://127.0.0.1:8123\nChanges sync on save."
	rendered := drawPanel(&state, terminalSize{width: 44, height: 20}).render(palette{})
	if !strings.Contains(rendered, "http://127.0.0.1:8123") {
		t.Fatalf("startup URL is below the initial output page: %q", rendered)
	}
}

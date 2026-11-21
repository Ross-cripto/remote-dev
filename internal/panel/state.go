package panel

import "strings"

type shortcut struct {
	key    key
	action string
	label  string
}

var actionShortcuts = []shortcut{
	{'s', "start", "Start"},
	{'u', "sync", "Sync"},
	{'x', "stop", "Stop"},
	{'i', "status", "Status"},
	{'l', "logs", "Logs"},
	{'o', "open", "Open"},
}

type panelState struct {
	workspaces    []Workspace
	selectedIndex int
	pendingLabel  string
	message       string
	output        string
	failed        bool
	outputOffset  int
	outputPage    int
}

func (state *panelState) selected() Workspace {
	if len(state.workspaces) == 0 {
		return Workspace{}
	}
	return state.workspaces[state.selectedIndex]
}

func (state *panelState) refresh() command {
	return state.request(command{action: "refresh", label: "Load workspaces"})
}

func (state *panelState) finish(result jobResult) {
	completed := result.command
	state.pendingLabel = ""
	state.output = result.output
	if result.err != nil {
		state.message = completed.label + " · Failed"
		if strings.TrimSpace(state.output) != "" {
			state.output = strings.TrimRight(state.output, "\r\n") + "\n"
		}
		state.output += result.err.Error()
		state.failed = true
		return
	}
	if completed.action == "refresh" {
		state.replaceWorkspaces(result.workspaces)
		state.message = "Workspaces refreshed"
		return
	}
	state.message = completed.label + " · Finished"
}

func (state *panelState) replaceWorkspaces(workspaces []Workspace) {
	selectedRoot := state.selected().Root
	state.workspaces = workspaces
	state.selectedIndex = 0
	for index, workspace := range workspaces {
		if workspace.Root == selectedRoot {
			state.selectedIndex = index
			return
		}
	}
}

func (state *panelState) press(pressed key) (command, bool) {
	switch pressed {
	case keyInterrupt:
		return command{}, true
	case 'q':
		if state.pendingLabel == "" {
			return command{}, true
		}
		state.message = "Wait for " + state.pendingLabel + " before leaving."
	case keyUp:
		state.selectedIndex = max(0, state.selectedIndex-1)
	case keyDown:
		if len(state.workspaces) > 0 {
			state.selectedIndex = min(len(state.workspaces)-1, state.selectedIndex+1)
		}
	case keyPageUp, '[':
		state.outputOffset = max(0, state.outputOffset-state.outputPage)
	case keyPageDown, ']':
		state.outputOffset += state.outputPage
	case 'r':
		return state.refresh(), false
	default:
		for _, shortcut := range actionShortcuts {
			if pressed == shortcut.key {
				selected := state.selected()
				label := shortcut.label + " · " + selected.Name
				return state.request(command{action: shortcut.action, label: label, workspace: selected}), false
			}
		}
	}
	return command{}, false
}

func (state *panelState) request(next command) command {
	if state.pendingLabel != "" {
		state.message = state.pendingLabel + " · Still in progress"
		return command{}
	}
	if next.action != "refresh" && next.workspace.Root == "" {
		state.message = "Choose a workspace first. Press r to refresh."
		return command{}
	}
	state.pendingLabel = next.label
	state.message = next.label + " · In progress"
	state.output = ""
	state.outputOffset = 0
	state.failed = false
	return next
}

package panel

import (
	"fmt"
	"os"
	"strings"
)

const (
	panelTitle             = "Remote Dev"
	wideColumns            = 76
	minimumColumns         = 28
	wideBodyTop            = 4
	stackedBodyTop         = 3
	outerMargin            = 2
	stackedWorkspaceLimit  = 4
	stackedDetailRows      = 2
	wideDetailRows         = 4
	activityHeaderRows     = 2
	workspaceHeadingRows   = 1
	minimumStackedBodyRows = workspaceHeadingRows + 1 + stackedDetailRows + activityHeaderRows + 1
	minimumCompactBodyRows = 4
)

var navigationHints = []string{"[↑/↓] Select", "[PgUp/Dn] Output", "[r] Refresh", "[q] Quit"}

type tone uint8

const (
	textTone tone = iota
	accentTone
	mutedTone
	errorTone
)

type palette struct {
	background string
	colors     [4]string
}

func terminalPalette() palette {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return palette{}
	}
	switch os.Getenv("COLORTERM") {
	case "truecolor", "24bit":
		return palette{
			background: "\x1b[48;2;23;25;26m",
			colors: [4]string{
				textTone: "\x1b[22;38;2;245;241;232m", accentTone: "\x1b[1;38;2;255;134;91m",
				mutedTone: "\x1b[22;38;2;181;173;165m", errorTone: "\x1b[1;38;2;255;164;154m",
			},
		}
	default:
		if strings.Contains(os.Getenv("TERM"), "256color") {
			return palette{
				background: "\x1b[48;5;234m",
				colors: [4]string{
					textTone: "\x1b[22;38;5;255m", accentTone: "\x1b[1;38;5;209m",
					mutedTone: "\x1b[22;38;5;248m", errorTone: "\x1b[1;38;5;217m",
				},
			}
		}
		return palette{colors: [4]string{accentTone: "\x1b[1;33m", errorTone: "\x1b[1;31m", textTone: "\x1b[22;39m", mutedTone: "\x1b[22;39m"}}
	}
}

type terminalSize struct {
	width  int
	height int
}

type span struct {
	row    int
	column int
	text   string
	tone   tone
}

type frame struct {
	size  terminalSize
	spans []span
}

func (frame *frame) write(row, column, width int, value string, color tone) {
	if row < 0 || row >= frame.size.height || column < 0 {
		return
	}
	width = min(width, frame.size.width-column)
	if width < 1 {
		return
	}
	visible := wrapCells(stripEscapes(value), width)[0]
	if visible != "" {
		frame.spans = append(frame.spans, span{row: row, column: column, text: visible, tone: color})
	}
}

func (frame frame) render(style palette) string {
	var output strings.Builder
	output.WriteString(style.background)
	output.WriteString("\x1b[H\x1b[2J")
	for _, span := range frame.spans {
		fmt.Fprintf(&output, "\x1b[%d;%dH%s%s", span.row+1, span.column+1, style.colors[span.tone], span.text)
	}
	output.WriteString("\x1b[0m")
	return output.String()
}

func drawPanel(state *panelState, size terminalSize) frame {
	frame := frame{size: size}
	width := size.width - outerMargin*2
	hints := shortcutRows(width)
	bottom := size.height - len(hints) - 1
	wide := size.width >= wideColumns
	minimumBottom := stackedBodyTop + minimumStackedBodyRows
	if wide {
		minimumBottom = wideBodyTop + wideDetailRows + activityHeaderRows + 1
	}
	if size.width < minimumColumns || bottom < minimumBottom {
		drawCompact(&frame, state)
		return frame
	}

	frame.write(1, outerMargin, width, panelTitle, accentTone)
	frame.write(2, outerMargin, width, strings.Repeat("─", width), mutedTone)
	if wide {
		drawWide(&frame, state, width, bottom)
	} else {
		drawStacked(&frame, state, width, bottom)
	}
	frame.write(bottom, outerMargin, width, strings.Repeat("─", width), mutedTone)
	drawHints(&frame, hints, bottom+1, outerMargin, width)
	return frame
}

func drawHints(frame *frame, hints []string, top, column, width int) {
	for index, hint := range hints {
		frame.write(top+index, column, width, hint, mutedTone)
	}
}

func shortcutRows(width int) []string {
	var actionHints []string
	for _, shortcut := range actionShortcuts {
		actionHints = append(actionHints, fmt.Sprintf("[%c] %s", shortcut.key, shortcut.label))
	}
	rows := packHints(actionHints, width)
	return append(rows, packHints(navigationHints, width)...)
}

func packHints(hints []string, width int) []string {
	var rows []string
	var current string
	for _, hint := range hints {
		if current == "" {
			current = hint
			continue
		}
		next := current + "   " + hint
		if cellWidth(next) <= width {
			current = next
			continue
		}
		rows = append(rows, current)
		current = hint
	}
	return append(rows, current)
}

func drawCompact(frame *frame, state *panelState) {
	width := frame.size.width
	height := frame.size.height
	frame.write(0, 0, width, panelTitle, accentTone)
	if height < 2 {
		return
	}
	bottom := height - 1
	hints := shortcutRows(width)
	if height-len(hints) >= minimumCompactBodyRows && width >= minimumColumns {
		bottom = height - len(hints)
		drawHints(frame, hints, bottom, 0, width)
	} else {
		frame.write(bottom, 0, width, "q Quit   r Refresh", mutedTone)
	}
	if bottom > 1 {
		frame.write(1, 0, width, state.selected().Name, accentTone)
	}
	if bottom > 2 {
		drawActivity(frame, state, 2, bottom, 0, width, false)
	}
}

func drawWide(frame *frame, state *panelState, width, bottom int) {
	workspaceWidth := width / 3
	detailColumn := outerMargin + workspaceWidth + 3
	detailWidth := width - workspaceWidth - 3
	for row := wideBodyTop; row < bottom; row++ {
		frame.write(row, detailColumn-2, 1, "│", mutedTone)
	}
	drawWorkspaces(frame, state, wideBodyTop, workspaceWidth, bottom-wideBodyTop-1)
	selected := state.selected()
	frame.write(wideBodyTop, detailColumn, detailWidth, selected.Name, accentTone)
	drawDetails(frame, selected, wideBodyTop+1, detailColumn, detailWidth)
	drawActivity(frame, state, wideBodyTop+wideDetailRows, bottom, detailColumn, detailWidth, true)
}

func drawStacked(frame *frame, state *panelState, width, bottom int) {
	available := bottom - stackedBodyTop - minimumStackedBodyRows + 1
	workspaceRows := min(stackedWorkspaceLimit, available, max(1, len(state.workspaces)))
	drawWorkspaces(frame, state, stackedBodyTop, width, workspaceRows)
	detailsTop := stackedBodyTop + workspaceRows + workspaceHeadingRows
	drawDetails(frame, state.selected(), detailsTop, outerMargin, width)
	drawActivity(frame, state, detailsTop+stackedDetailRows, bottom, outerMargin, width, true)
}

func drawWorkspaces(frame *frame, state *panelState, top, width, limit int) {
	count := len(state.workspaces)
	heading := "Workspaces"
	if count > limit {
		heading += fmt.Sprintf("  %d/%d", state.selectedIndex+1, count)
	}
	frame.write(top, outerMargin, width, heading, accentTone)
	if count == 0 {
		empty := "No worktrees found"
		if state.pendingLabel != "" {
			empty = "Discovering worktrees…"
		}
		frame.write(top+1, outerMargin, width, empty, mutedTone)
		return
	}

	first := max(0, state.selectedIndex-limit+1)
	for index := first; index < min(first+limit, count); index++ {
		workspace := state.workspaces[index]
		line := workspace.Name
		if workspace.Current {
			line += " (current)"
		}
		color := textTone
		prefix := "  "
		if index == state.selectedIndex {
			color = accentTone
			prefix = "› "
		}
		frame.write(top+workspaceHeadingRows+index-first, outerMargin, width, prefix+line, color)
	}
}

func drawDetails(frame *frame, workspace Workspace, top, column, width int) {
	if workspace.Root == "" {
		return
	}
	details := "Host  " + workspace.Host + "    Mode  " + workspace.Mode
	frame.write(top, column, width, details, textTone)
	frame.write(top+1, column, width, "Path  "+workspace.Root, mutedTone)
}

func drawActivity(frame *frame, state *panelState, top, bottom, column, width int, heading bool) {
	messageRow := top
	if heading {
		messageRow++
	}
	outputTop := messageRow + 1
	state.outputPage = max(1, bottom-outputTop)
	lines := outputLines(state.output, width)
	state.outputOffset = min(state.outputOffset, max(0, len(lines)-state.outputPage))
	end := min(len(lines), state.outputOffset+max(0, bottom-outputTop))
	if heading {
		title := "Activity"
		if len(lines) > state.outputPage {
			title += fmt.Sprintf("  %d–%d/%d", state.outputOffset+1, end, len(lines))
		}
		frame.write(top, column, width, title, accentTone)
	}
	color := textTone
	if state.failed {
		color = errorTone
	} else if state.pendingLabel != "" {
		color = accentTone
	}
	frame.write(messageRow, column, width, state.message, color)
	for index := state.outputOffset; index < end; index++ {
		frame.write(outputTop+index-state.outputOffset, column, width, lines[index], textTone)
	}
}

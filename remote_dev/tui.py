from __future__ import annotations

import curses
import re
import unicodedata
from collections.abc import Callable, Iterator
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path


_ACTIONS = {
    ord("s"): ("up", "Start"),
    ord("u"): ("sync", "Sync"),
    ord("x"): ("stop", "Stop"),
    ord("i"): ("status", "Status"),
    ord("l"): ("logs", "Logs"),
    ord("o"): ("open", "Open"),
}
_ACTION_KEYS = tuple(f"[{chr(key)}] {label}" for key, (_, label) in _ACTIONS.items())
_NAVIGATION_KEYS = ("[↑/↓] Select", "[PgUp/Dn] Output", "[r] Refresh", "[q] Quit")
_POLL_MS = 100
_WIDE_COLUMNS = 92
_MINIMUM_COLUMNS = 28
_BODY_TOP = 4
_STACKED_FIXED_ROWS = 9
_MAX_STACKED_WORKSPACE_ROWS = 6
_ANSI_ESCAPE = re.compile(r"\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\))")


@dataclass(frozen=True)
class Workspace:
    path: Path
    name: str
    host: str
    mode: str


@dataclass(frozen=True)
class _Job:
    label: str
    future: Future[str] | Future[list[Workspace]]


class _Panel:
    def __init__(
        self,
        discover: Callable[[], list[Workspace]],
        invoke: Callable[[str, Path], str],
        worker: ThreadPoolExecutor,
    ) -> None:
        self.discover = discover
        self.invoke = invoke
        self.worker = worker
        self.workspaces: list[Workspace] = []
        self.selected_index = 0
        self.job: _Job | None = None
        self.message = ""
        self.output = ""
        self.failed = False
        self.output_offset = 0
        self.output_page = 1

    @property
    def selected(self) -> Workspace | None:
        if self.workspaces:
            return self.workspaces[self.selected_index]
        return None

    def refresh(self) -> None:
        self._start(_Job("Load workspaces", self.worker.submit(self.discover)))

    def _start(self, job: _Job) -> None:
        self.job = job
        self.message = f"{job.label} · In progress"
        self.output = ""
        self.output_offset = 0
        self.failed = False

    def poll(self) -> bool:
        job = self.job
        if job is None or not job.future.done():
            return False
        self.job = None
        try:
            result = job.future.result()
        except (OSError, RuntimeError, ValueError) as error:
            self.message = f"{job.label} · Failed"
            self.output = str(error)
            self.failed = True
        else:
            if isinstance(result, str):
                self.message = f"{job.label} · Finished"
                self.output = result
            else:
                self._replace_workspaces(result)
                self.message = "Workspaces refreshed"
        return True

    def _replace_workspaces(self, workspaces: list[Workspace]) -> None:
        selected = self.selected
        self.workspaces = workspaces
        self.selected_index = 0
        if selected is not None:
            for index, workspace in enumerate(workspaces):
                if workspace.path == selected.path:
                    self.selected_index = index
                    break

    def handle(self, key: int) -> bool:
        if key == ord("q"):
            if self.job is None:
                return False
            self.message = f"Wait for {self.job.label} before leaving."
        elif key in (curses.KEY_UP, curses.KEY_DOWN) and self.workspaces:
            delta = 1 if key == curses.KEY_DOWN else -1
            self.selected_index = max(
                0, min(self.selected_index + delta, len(self.workspaces) - 1)
            )
        elif key in (curses.KEY_PPAGE, ord("[")):
            self.output_offset = max(0, self.output_offset - self.output_page)
        elif key in (curses.KEY_NPAGE, ord("]")):
            self.output_offset += self.output_page
        elif key == ord("r") or key in _ACTIONS:
            self._handle_action(key)
        return True

    def _handle_action(self, key: int) -> None:
        if self.job is not None:
            self.message = f"{self.job.label} · Still in progress"
            return
        if key == ord("r"):
            self.refresh()
            return
        selected = self.selected
        if selected is None:
            self.message = "Choose a workspace first. Press r to refresh."
            return
        action, label = _ACTIONS[key]
        name = f"{label} · {selected.name}"
        self._start(_Job(name, self.worker.submit(self.invoke, action, selected.path)))


@dataclass(frozen=True)
class _Styles:
    body: int = curses.A_NORMAL
    heading: int = curses.A_BOLD
    muted: int = curses.A_NORMAL
    selected: int = curses.A_REVERSE | curses.A_BOLD
    busy: int = curses.A_BOLD
    success: int = curses.A_BOLD
    error: int = curses.A_BOLD


def _styles() -> _Styles:
    if not curses.has_colors() or curses.COLORS < 256 or curses.COLOR_PAIRS < 8:
        return _Styles()
    background = 234
    try:
        curses.init_pair(1, 252, background)
        curses.init_pair(2, 117, background)
        curses.init_pair(3, 248, background)
        curses.init_pair(4, 231, 24)
        curses.init_pair(5, 222, background)
        curses.init_pair(6, 114, background)
        curses.init_pair(7, 210, background)
    except curses.error:
        return _Styles()
    return _Styles(
        body=curses.color_pair(1),
        heading=curses.color_pair(2) | curses.A_BOLD,
        muted=curses.color_pair(3),
        selected=curses.color_pair(4) | curses.A_BOLD,
        busy=curses.color_pair(5) | curses.A_BOLD,
        success=curses.color_pair(6) | curses.A_BOLD,
        error=curses.color_pair(7) | curses.A_BOLD,
    )


def _cell_chunks(value: str, width: int) -> Iterator[str]:
    if width < 1:
        return
    characters: list[str] = []
    occupied = 0
    for character in _ANSI_ESCAPE.sub("", value).expandtabs():
        if not character.isprintable():
            continue
        if unicodedata.category(character) in ("Mn", "Me"):
            if characters:
                characters.append(character)
            continue
        character_width = (
            2 if unicodedata.east_asian_width(character) in ("W", "F") else 1
        )
        if character_width > width:
            character, character_width = "?", 1
        if occupied + character_width > width:
            yield "".join(characters)
            occupied = 0
            characters = []
        occupied += character_width
        characters.append(character)
    yield "".join(characters)


def _shortcut_rows(shortcuts: tuple[str, ...], width: int) -> Iterator[str]:
    line = ""
    for shortcut in shortcuts:
        if not line:
            line = shortcut
        elif len(line) + len(shortcut) + 3 <= width:
            line = f"{line}   {shortcut}"
        else:
            yield line
            line = shortcut
    if line:
        yield line


class _View:
    def __init__(self, screen: curses.window, title: str) -> None:
        self.screen = screen
        self.title = title
        self.styles = _styles()
        if self.styles.body:
            self.screen.bkgd(" ", self.styles.body)

    def write(
        self, row: int, column: int, width: int, value: str, style: int = 0
    ) -> None:
        rows, columns = self.screen.getmaxyx()
        if not (0 <= row < rows and 0 <= column < columns - 1):
            return
        available = min(width, columns - column - 1)
        visible = next(_cell_chunks(value, available), "")
        if not visible:
            return
        try:
            self.screen.addstr(row, column, visible, style or self.styles.body)
        except curses.error:
            return

    def draw(self, panel: _Panel) -> None:
        rows, columns = self.screen.getmaxyx()
        self.screen.erase()
        margin = 2 if columns >= _MINIMUM_COLUMNS else 0
        width = columns - 2 * margin - 1
        footer = [
            *_shortcut_rows(_ACTION_KEYS, width),
            *_shortcut_rows(_NAVIGATION_KEYS, width),
        ]
        body_bottom = rows - len(footer) - 1
        if columns < _MINIMUM_COLUMNS or body_bottom - _BODY_TOP <= _STACKED_FIXED_ROWS:
            self.write(0, margin, width, self.title, self.styles.heading)
            selected = panel.selected
            if selected is not None and rows > 2:
                self.write(1, margin, width, selected.name, self.styles.selected)
            if rows > 3:
                self.write(2, margin, width, panel.message, self._activity_style(panel))
            if rows > 1:
                self.write(
                    rows - 1, margin, width, "q Quit   r Refresh", self.styles.muted
                )
        else:
            self.write(1, margin, width, self.title, self.styles.heading)
            self.write(2, margin, width, "─" * width, self.styles.muted)
            if columns >= _WIDE_COLUMNS:
                self._draw_wide(panel, margin, width, body_bottom)
            else:
                self._draw_stacked(panel, margin, width, body_bottom)
            self.write(body_bottom, margin, width, "─" * width, self.styles.muted)
            for row, line in enumerate(footer, start=body_bottom + 1):
                self.write(row, margin, width, line, self.styles.muted)
        self.screen.refresh()

    def _draw_wide(self, panel: _Panel, margin: int, width: int, bottom: int) -> None:
        top = _BODY_TOP
        workspace_width = width // 3
        for row in range(top, bottom):
            self.write(row, margin + workspace_width + 1, 1, "│", self.styles.muted)
        self._draw_workspaces(panel, top, margin, workspace_width, bottom - top - 1)
        detail_column = margin + workspace_width + 4
        detail_width = width - workspace_width - 4
        self._draw_details(panel, top, detail_column, detail_width)
        self._draw_activity(panel, top + 4, bottom, detail_column, detail_width)

    def _draw_stacked(
        self, panel: _Panel, margin: int, width: int, bottom: int
    ) -> None:
        top = _BODY_TOP
        workspace_rows = min(
            bottom - top - _STACKED_FIXED_ROWS, _MAX_STACKED_WORKSPACE_ROWS
        )
        visible = max(1, min(len(panel.workspaces), workspace_rows))
        self._draw_workspaces(panel, top, margin, width, visible)
        details_top = top + visible + 2
        self._draw_details(panel, details_top, margin, width)
        self._draw_activity(panel, details_top + 4, bottom, margin, width)

    def _draw_workspaces(
        self, panel: _Panel, top: int, column: int, width: int, visible: int
    ) -> None:
        count = len(panel.workspaces)
        heading = (
            f"Workspaces  {panel.selected_index + 1}/{count}" if count else "Workspaces"
        )
        self.write(top, column, width, heading, self.styles.heading)
        if not count:
            empty = (
                "Discovering workspaces…"
                if panel.job is not None
                else "No workspaces found"
            )
            self.write(top + 1, column, width, empty, self.styles.muted)
            return
        first = max(0, panel.selected_index - visible + 1)
        for index in range(first, min(first + visible, count)):
            selected = index == panel.selected_index
            prefix = "› " if selected else "  "
            style = self.styles.selected if selected else self.styles.body
            self.write(
                top + index - first + 1,
                column,
                width,
                prefix + panel.workspaces[index].name,
                style,
            )

    def _draw_details(self, panel: _Panel, top: int, column: int, width: int) -> None:
        selected = panel.selected
        if selected is None:
            return
        self.write(top, column, width, selected.name, self.styles.heading)
        self.write(
            top + 1, column, width, f"Host  {selected.host}    Mode  {selected.mode}"
        )
        self.write(top + 2, column, width, f"Path  {selected.path}", self.styles.muted)

    def _draw_activity(
        self, panel: _Panel, top: int, bottom: int, column: int, width: int
    ) -> None:
        self.write(top, column, width, "Activity", self.styles.heading)
        self.write(top + 1, column, width, panel.message, self._activity_style(panel))
        output_lines = panel.output.strip("\r\n").splitlines()
        lines = [chunk for line in output_lines for chunk in _cell_chunks(line, width)]
        top += 2
        panel.output_page = max(1, bottom - top)
        panel.output_offset = min(
            panel.output_offset, max(0, len(lines) - panel.output_page)
        )
        end = min(len(lines), panel.output_offset + panel.output_page)
        for row, line in enumerate(lines[panel.output_offset : end], start=top):
            self.write(row, column, width, line)

    def _activity_style(self, panel: _Panel) -> int:
        if panel.failed:
            return self.styles.error
        if panel.job is not None:
            return self.styles.busy
        return self.styles.success


def launch(
    title: str,
    discover: Callable[[], list[Workspace]],
    invoke: Callable[[str, Path], str],
) -> None:
    def run(screen: curses.window) -> None:
        try:
            curses.curs_set(0)
        except curses.error:
            pass
        view = _View(screen, title)
        with ThreadPoolExecutor(
            max_workers=1, thread_name_prefix="remote-dev"
        ) as worker:
            panel = _Panel(discover, invoke, worker)
            panel.refresh()
            dirty = True
            while True:
                if panel.poll() or dirty:
                    view.draw(panel)
                screen.timeout(_POLL_MS if panel.job is not None else -1)
                key = screen.getch()
                dirty = key != -1
                if dirty and not panel.handle(key):
                    return

    curses.wrapper(run)

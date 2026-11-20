import curses
import time
import unittest
from collections import deque
from pathlib import Path
from threading import Event
from unittest.mock import patch

from remote_dev.tui import Workspace, launch


WAIT_SECONDS = 3
FIXTURE_WORKSPACES = [
    Workspace(Path("/work/current"), "Current worktree", "current-host", "remote"),
    Workspace(Path("/work/topic"), "Topic worktree", "topic-host", "remote"),
]


class Screen:
    def __init__(self, inputs, *, rows=24, columns=80):
        self.inputs = deque(inputs)
        self.rows = rows
        self.columns = columns
        self.frame = {}
        self.deadline = time.monotonic() + WAIT_SECONDS

    def getmaxyx(self):
        return self.rows, self.columns

    def erase(self):
        self.frame.clear()

    def addstr(self, row, column, value, style):
        assert value and value.isprintable(), repr(value)
        assert 0 <= row < self.rows, row
        assert 0 <= column < self.columns - 1, column
        assert column + len(value) < self.columns, value
        previous = self.frame.get(row, " " * self.columns)
        end = column + len(value)
        self.frame[row] = previous[:column] + value + previous[end:]

    def refresh(self):
        pass

    def timeout(self, milliseconds):
        pass

    def getch(self):
        marker, key = self.inputs[0]
        if marker is None or any(marker in line for line in self.frame.values()):
            self.inputs.popleft()
            if callable(key):
                return key()
            return key
        if time.monotonic() > self.deadline:
            raise AssertionError(f"Timed out waiting for {marker!r} in {self.frame!r}")
        time.sleep(0.001)
        return -1


def run_panel(screen, discover, invoke):
    with (
        patch.object(curses, "wrapper", side_effect=lambda run: run(screen)),
        patch.object(curses, "has_colors", return_value=False),
        patch.object(curses, "curs_set"),
    ):
        launch("remote-dev", discover, invoke)


class TerminalPanelTest(unittest.TestCase):
    def test_keyboard_stays_responsive_while_an_action_finishes(self):
        allow_completion = Event()
        calls = []

        def invoke(action, path):
            calls.append((action, path))
            if not allow_completion.wait(WAIT_SECONDS):
                raise RuntimeError("Action was blocked")
            return "Workspace status received"

        def finish_action():
            allow_completion.set()
            return -1

        screen = Screen(
            [
                ("current-host", curses.KEY_DOWN),
                ("topic-host", ord("i")),
                (None, curses.KEY_UP),
                ("current-host", ord("x")),
                (None, ord("q")),
                ("before leaving", finish_action),
                ("Workspace status received", ord("q")),
            ]
        )
        run_panel(screen, lambda: FIXTURE_WORKSPACES, invoke)
        self.assertEqual(calls, [("status", Path("/work/topic"))])
        self.assertFalse(screen.inputs)

    def test_refresh_preserves_the_selected_workspace_path(self):
        inventories = deque([FIXTURE_WORKSPACES, list(reversed(FIXTURE_WORKSPACES))])
        calls = []

        def invoke(action, path):
            calls.append((action, path))
            return "Opened workspace"

        screen = Screen(
            [
                ("current-host", curses.KEY_DOWN),
                ("topic-host", ord("r")),
                ("Workspaces refreshed", ord("o")),
                ("Opened workspace", ord("q")),
            ]
        )
        run_panel(screen, inventories.popleft, invoke)
        self.assertEqual(calls, [("open", Path("/work/topic"))])
        self.assertFalse(inventories)

    def test_tiny_resize_and_blank_control_output_stay_writable(self):
        def shrink():
            screen.rows = 1
            screen.columns = 1
            return curses.KEY_RESIZE

        def restore():
            screen.rows = 20
            screen.columns = 44
            return curses.KEY_RESIZE

        screen = Screen(
            [
                ("current-host", shrink),
                (None, restore),
                ("current-host", ord("l")),
                ("Log output", ord("q")),
            ]
        )
        output = "\n\x1b[31mLog output\x1b[0m\n\n"
        workspaces = [
            *FIXTURE_WORKSPACES,
            Workspace(Path("/work/third"), "Third worktree", "third-host", "remote"),
        ]
        run_panel(screen, lambda: workspaces, lambda action, path: output)
        self.assertFalse(screen.inputs)

    def test_action_errors_are_visible_and_allow_another_action(self):
        def invoke(action, path):
            if action == "sync":
                raise RuntimeError("SSH connection failed")
            return "Status is ready"

        screen = Screen(
            [
                ("current-host", ord("u")),
                ("SSH connection failed", ord("i")),
                ("Status is ready", ord("q")),
            ],
            columns=120,
        )
        run_panel(screen, lambda: FIXTURE_WORKSPACES, invoke)
        self.assertFalse(screen.inputs)

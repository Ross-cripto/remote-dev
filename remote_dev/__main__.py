from __future__ import annotations

import argparse
from pathlib import Path
import sys

from . import __version__
from .client import Client
from .config import init_project
from .connection import onboard
from .snapshots import find_worktree


OPERATIONS = {
    "up": "Copy current source, start services and open SSH forwards",
    "sync": "Copy source and refresh services if they are running",
    "status": "Show the current workspace services and URLs",
    "logs": "Show recent service logs",
    "stop": "Stop only the selected workspace and its tunnel",
    "open": "Open the first forwarded URL in the browser",
    "doctor": "Check SSH, Python and Docker Compose on the host",
}


def parser() -> argparse.ArgumentParser:
    command = argparse.ArgumentParser(
        prog="remote-dev",
        description="Run your development services on a Linux box over SSH.",
    )
    command.add_argument("--version", action="version", version=__version__)
    actions = command.add_subparsers(dest="action")
    connection = actions.add_parser("connect", help="Check and remember a Linux server")
    connection.add_argument("target", nargs="?", help="OpenSSH alias or user@server")
    connection.add_argument("--name", help="Label to remember for this server")
    connection.add_argument("--identity", help="Path to the local SSH identity file")
    connection.add_argument(
        "--prepare",
        action="store_true",
        help="Offer to install missing server prerequisites",
    )
    initialize = actions.add_parser(
        "init", help="Write configuration for this Git worktree"
    )
    initialize.add_argument("--host", required=True, help="OpenSSH host alias")
    initialize.add_argument(
        "--mode", choices=("compose", "commands", "node"), default="compose"
    )
    for action, description in OPERATIONS.items():
        operation = actions.add_parser(action, help=description)
        operation.add_argument(
            "--host", help="Override the SSH alias from remote-dev.toml"
        )
        if action == "up":
            operation.add_argument(
                "--no-watch",
                action="store_true",
                help="Start without syncing changes on save",
            )
    actions.add_parser("list", help="List workspaces used by this client")
    actions.add_parser("tui", help="Open the workspace panel")
    return command


def terminal_panel(client: Client) -> None:
    from .tui import Workspace, launch

    root = find_worktree(Path.cwd())

    def discover() -> list[Workspace]:
        try:
            project = client.project(root, None)
            host, mode = project.host, project.mode
        except ValueError:
            host, mode = client.default_host() or "Connect a server", "Auto"
        workspaces = [Workspace(path=root, name=root.name, host=host, mode=mode)]
        seen = {root}
        for entry in client.entries():
            path = Path(entry["path"])
            if path in seen or not path.is_dir():
                continue
            workspaces.append(
                Workspace(
                    path=path, name=path.name, host=entry["host"], mode=entry["mode"]
                )
            )
            seen.add(path)
        return workspaces

    launch("Remote Dev", discover, lambda action, path: client.execute(action, path))


def main(argv: list[str] | None = None) -> int:
    command = parser()
    arguments = command.parse_args(argv)
    try:
        if arguments.action == "connect":
            print(
                onboard(
                    Client().state,
                    arguments.target,
                    name=arguments.name,
                    identity=arguments.identity,
                    prepare=arguments.prepare,
                )
            )
        elif arguments.action == "init":
            path = find_worktree(Path.cwd())
            init_project(path, arguments.host, arguments.mode)
            print(
                f"Created {path / 'remote-dev.toml'}. Edit it, then run remote-dev doctor and remote-dev up."
            )
        elif arguments.action == "list":
            rows = Client().entries()
            if not rows:
                print("No workspaces yet. Run remote-dev up from a Git worktree.")
            for entry in rows:
                print(f"{entry['host']}  {entry['mode']}  {entry['path']}")
        elif arguments.action is None or arguments.action == "tui":
            if not sys.stdin.isatty() or not sys.stdout.isatty():
                command.print_help()
                return 0
            terminal_panel(Client(quiet=True))
        else:
            print(
                Client().execute(
                    arguments.action,
                    Path.cwd(),
                    arguments.host,
                    watch_enabled=not getattr(arguments, "no_watch", False),
                )
            )
    except (OSError, ValueError, RuntimeError) as error:
        print(f"remote-dev: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

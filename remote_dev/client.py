from __future__ import annotations

from contextlib import contextmanager
from dataclasses import asdict, replace
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shlex
import socket
import stat
import subprocess
from typing import BinaryIO, Iterator
import uuid
import webbrowser

from . import watch
from .config import CONFIG_FILE, Forward, Project, load_project
from .connection import Connection, load_connection, ssh_options
from .detection import detect_project
from .snapshots import capture, find_worktree


ADDRESS = "127.0.0.1"
STATE_SUBDIRECTORY = "remote-dev/standalone-v1"
HOST_SCRIPT = Path(__file__).with_name("host.py")


def storage(environment_key: str, default: str) -> Path:
    return (
        Path(os.environ.get(environment_key) or Path.home() / default)
        / STATE_SUBDIRECTORY
    )


def write_json(path: Path, record: dict) -> None:
    temporary = path.with_suffix(".tmp")
    with open(temporary, "w", opener=private_open) as stream:
        json.dump(record, stream, indent=2)
    temporary.replace(path)


def private_open(path, flags):
    return os.open(path, flags | os.O_NOFOLLOW, 0o600)


def lookup_port(preferred: int | None = None) -> int:
    with socket.socket() as probe:
        probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        probe.bind((ADDRESS, preferred or 0))
        return probe.getsockname()[1]


class Client:
    def __init__(self, *, quiet: bool = False, state: Path | None = None):
        self.state = (
            state if state is not None else storage("XDG_STATE_HOME", ".local/state")
        )
        self.cache = storage("XDG_CACHE_HOME", ".cache")
        self.quiet = quiet

    @contextmanager
    def locked(self) -> Iterator[None]:
        self.state.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.cache.mkdir(mode=0o700, parents=True, exist_ok=True)
        with open(self.state / "operation.lock", "a", opener=private_open) as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            yield

    def key(self, root: Path, host: str) -> str:
        identity_path = self.state / "client.json"
        if not identity_path.exists():
            write_json(identity_path, {"id": uuid.uuid4().hex})
        identity = json.loads(identity_path.read_text())["id"]
        return hashlib.sha256(os.fsencode(f"{identity}\0{host}\0{root}")).hexdigest()[
            :24
        ]

    def entries(self) -> list[dict]:
        return [
            json.loads(path.read_text())
            for path in sorted(self.state.glob("workspace-*.json"))
        ]

    def entry_path(self, key: str) -> Path:
        return self.state / f"workspace-{key}.json"

    def socket_path(self, key: str) -> Path:
        return self.cache / (key[:16] + ".sock")

    def connection(self, label: str) -> Connection:
        saved = load_connection(self.state, label)
        return saved[1] if saved else Connection(label)

    def default_host(self) -> str | None:
        saved = load_connection(self.state)
        return saved[0] if saved else None

    def remote(
        self,
        host: str,
        action: str,
        key: str = "",
        request: dict | None = None,
        source: BinaryIO | None = None,
    ) -> str:
        command = shlex.join(
            [
                "python3",
                "-c",
                HOST_SCRIPT.read_text(),
                action,
                key,
                json.dumps(request or {}),
            ]
        )
        connection = self.connection(host)
        result = subprocess.run(
            [
                "ssh",
                *ssh_options(connection),
                "-T",
                "-o",
                "ClearAllForwardings=yes",
                "-o",
                "ControlPath=none",
                "--",
                connection.target,
                command,
            ],
            stdin=source if source is not None else subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE if self.quiet else None,
        )
        if result.returncode:
            detail = ""
            if result.stderr:
                detail = result.stderr.decode(errors="replace").strip()
            raise RuntimeError(
                detail or f"{action} on {host} failed with exit {result.returncode}"
            )
        return result.stdout.decode().strip()

    def control(
        self, key: str, action: str, forwards: list[str] | None = None
    ) -> subprocess.CompletedProcess:
        return subprocess.run(
            [
                "ssh",
                "-F",
                "/dev/null",
                "-S",
                str(self.socket_path(key)),
                "-O",
                action,
                *(forwards or []),
                "localhost",
            ],
            capture_output=True,
            text=True,
        )

    def connected(self, key: str) -> bool:
        return self.control(key, "check").returncode == 0

    def disconnect(self, key: str) -> None:
        if self.connected(key):
            result = self.control(key, "exit")
            if result.returncode:
                raise RuntimeError(
                    result.stderr.strip() or "Failed to close workspace SSH tunnel"
                )

    def connect(
        self, host: str, key: str, local: dict[str, int], remote: dict[str, int]
    ) -> None:
        path = self.socket_path(key)
        if path.exists() or path.is_symlink():
            info = path.lstat()
            if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.getuid():
                raise RuntimeError(f"Unexpected file at SSH socket {path}")
            path.unlink()
        connection = self.connection(host)
        result = subprocess.run(
            [
                "ssh",
                *ssh_options(connection),
                "-o",
                "ClearAllForwardings=yes",
                "-o",
                "ControlPersist=no",
                "-M",
                "-S",
                str(path),
                "-fNT",
                "--",
                connection.target,
            ],
            capture_output=True,
            text=True,
        )
        if result.returncode:
            raise RuntimeError(
                result.stderr.strip() or "Failed to open the workspace SSH tunnel"
            )
        forwards = []
        for label, port in local.items():
            forwards.extend(["-L", f"{ADDRESS}:{port}:{ADDRESS}:{remote[label]}"])
        result = self.control(key, "forward", forwards)
        if result.returncode:
            self.disconnect(key)
            raise RuntimeError(
                result.stderr.strip() or "Could not bind the workspace's local ports"
            )

    def choose_ports(
        self, project: Project, entry: dict, connected: bool
    ) -> dict[str, int]:
        current = entry.get("local_ports", {})
        if connected and self.same_forward_preferences(project, current):
            return current
        allocated = {}
        probes = []
        try:
            for forward in project.forwards:
                previous = current.get(forward.name)
                desired = forward.local or previous
                if connected and desired == previous and desired is not None:
                    allocated[forward.name] = desired
                    continue
                probe = socket.socket()
                probes.append(probe)
                probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                try:
                    probe.bind((ADDRESS, desired or 0))
                except OSError as error:
                    if forward.local is not None:
                        raise RuntimeError(
                            f"Local port {forward.local} is in use for {forward.name}. Choose another port or set local = 'auto'."
                        ) from error
                    probe.bind((ADDRESS, 0))
                allocated[forward.name] = probe.getsockname()[1]
        finally:
            for probe in probes:
                probe.close()
        if len(set(allocated.values())) != len(allocated):
            raise RuntimeError(
                "Two forwards selected the same local port. Choose separate ports."
            )
        return allocated

    @staticmethod
    def same_forward_preferences(project: Project, current: dict[str, int]) -> bool:
        if set(current) != {forward.name for forward in project.forwards}:
            return False
        return all(
            forward.local is None or forward.local == current[forward.name]
            for forward in project.forwards
        )

    def deploy(
        self,
        action: str,
        root: Path,
        project: Project,
        key: str,
        entry: dict,
        *,
        watch_enabled: bool,
    ) -> str:
        connected = self.connected(key)
        local_ports = self.choose_ports(project, entry, connected)
        baseline = watch.fingerprint(root) if action == "up" and watch_enabled else None
        entry.update(path=str(root), host=project.host, mode=project.mode)
        write_json(self.entry_path(key), entry)
        with capture(root, project.generated_compose) as snapshot:
            request = {
                "digest": snapshot.digest,
                "compose_file": project.compose_file,
                "forwards": [asdict(forward) for forward in project.forwards],
                "auto_ports": project.auto_ports,
                "compose_override": project.compose_override,
                "compose_profiles": project.compose_profiles,
            }
            with snapshot.path.open("rb") as source:
                result = json.loads(
                    self.remote(project.host, action, key, request, source)
                )
        if not result["running"]:
            self.disconnect(key)
            return "Source synchronized. Workspace remains stopped."
        remote_ports = result["ports"]
        if not project.forwards and remote_ports:
            detected = tuple(Forward(name, name, 0, None) for name in remote_ports)
            local_ports = self.choose_ports(
                replace(project, forwards=detected), entry, connected
            )
        still_current = (
            connected
            and entry.get("local_ports") == local_ports
            and entry.get("remote_ports") == remote_ports
        )
        if not still_current:
            self.disconnect(key)
            if local_ports:
                self.connect(project.host, key, local_ports, remote_ports)
        entry.update(local_ports=local_ports, remote_ports=remote_ports)
        write_json(self.entry_path(key), entry)
        lines = ["Workspace running."]
        if project.detected_from:
            lines.append(project.detected_from)
        if local_ports:
            lines.append(self.links(local_ports))
        else:
            lines.append(
                "No app ports detected. Declare a TCP port to open a local preview."
            )
        if baseline is not None:
            watch.stop(self.state, key)
            watch.start(
                self.state,
                root,
                project.host,
                key,
                baseline,
                reconnect=bool(local_ports),
            )
            lines.append("Changes sync on save.")
        return "\n".join(lines)

    @staticmethod
    def links(ports: dict[str, int]) -> str:
        return "\n".join(
            f"{label}  http://{ADDRESS}:{port}" for label, port in ports.items()
        )

    def management_host(self, root: Path, override: str | None) -> str:
        if not (root / CONFIG_FILE).exists():
            return self.detected_host(root, override)
        try:
            return load_project(root, override).host
        except ValueError as error:
            matching_hosts = {
                entry["host"]
                for entry in self.entries()
                if entry["path"] == str(root)
                and (override is None or override == entry["host"])
            }
            if len(matching_hosts) == 1:
                return matching_hosts.pop()
            if len(matching_hosts) > 1:
                raise ValueError(
                    "Select the registered SSH alias with --host"
                ) from error
            raise

    def detected_host(self, root: Path, override: str | None) -> str:
        if override is not None:
            self.connection(override)
            return override
        matching_hosts = {
            entry["host"] for entry in self.entries() if entry["path"] == str(root)
        }
        if len(matching_hosts) == 1:
            return matching_hosts.pop()
        if len(matching_hosts) > 1:
            raise ValueError("Select the workspace's SSH host with --host")
        default = self.default_host()
        if default is None:
            raise ValueError(
                "Connect a server first with remote-dev connect user@server."
            )
        return default

    def project(self, root: Path, override: str | None) -> Project:
        if (root / CONFIG_FILE).exists():
            if override is not None:
                self.connection(override)
            return load_project(root, override)
        return detect_project(root, self.detected_host(root, override))

    def execute(
        self,
        action: str,
        directory: Path,
        host_override: str | None = None,
        *,
        watch_token: str | None = None,
        watch_enabled: bool = True,
    ) -> str:
        if action == "doctor" and host_override is not None:
            return self.remote(host_override, action)
        try:
            root = find_worktree(directory)
        except ValueError:
            if action == "doctor":
                default = self.default_host()
                if default is not None:
                    return self.remote(default, action)
                raise ValueError(
                    "Connect a server first with remote-dev connect user@server."
                ) from None
            raise
        project = None
        if action in {"up", "sync"}:
            project = self.project(root, host_override)
            host = project.host
        else:
            host = self.management_host(root, host_override)
        if action == "doctor":
            return self.remote(host, action)
        with self.locked():
            key = self.key(root, host)
            if watch_token is not None and not watch.current(
                self.state, key, watch_token
            ):
                return "Watch cancelled."
            if action == "stop" or (action == "up" and not watch_enabled):
                watch.stop(self.state, key)
            path = self.entry_path(key)
            entry = json.loads(path.read_text()) if path.exists() else {}
            if project is not None:
                return self.deploy(
                    action, root, project, key, entry, watch_enabled=watch_enabled
                )
            if action == "open":
                ports = entry.get("local_ports", {})
                if not ports or not self.connected(key):
                    raise RuntimeError(
                        "No connected app port. Run remote-dev up from this worktree."
                    )
                port = next(iter(ports.values()))
                url = f"http://{ADDRESS}:{port}"
                if not webbrowser.open(url):
                    raise RuntimeError(f"Could not open browser. Open {url}")
                return url
            result = self.remote(host, action, key)
            if action == "stop":
                self.disconnect(key)
            if action == "status":
                if entry.get("local_ports") and self.connected(key):
                    result += "\n\n" + self.links(entry["local_ports"])
                if watch.active(self.state, key):
                    result += "\n\nChanges sync on save."
                    latest = watch.tail(self.state, key)
                    if latest:
                        result += "\n" + latest
            return result

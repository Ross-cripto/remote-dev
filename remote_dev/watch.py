from __future__ import annotations

from contextlib import contextmanager
from datetime import datetime
import fcntl
import hashlib
import os
from pathlib import Path
import subprocess
import sys
import threading
import time
from typing import Iterator
import uuid

from .snapshots import identity, source_paths


POLL_SECONDS = 0.75
DEBOUNCE_SECONDS = 1.0
CONNECTION_CHECK_SECONDS = 5.0
MAX_SAVE_ATTEMPTS = 3
RETRY_SECONDS = (2, 5, 10, 30, 60)
MAX_EVENT_LENGTH = 500


def fingerprint(root: Path) -> str:
    digest = hashlib.sha256()
    for relative in source_paths(root):
        try:
            source_identity = identity((root / relative).lstat())
        except FileNotFoundError:
            continue
        digest.update(os.fsencode(relative) + b"\0")
        digest.update(str(source_identity).encode("ascii") + b"\0")
    return digest.hexdigest()


class SyncSchedule:
    def __init__(self, baseline: str):
        self.applied = baseline
        self.observed = baseline
        self.settled_at = 0.0
        self.retry_at = 0.0
        self.attempts = 0
        self.connection_lost = False

    def observe(self, observed: str, now: float) -> None:
        if observed != self.observed:
            self.observed = observed
            self.settled_at = now + DEBOUNCE_SECONDS
            if not self.connection_lost:
                self.reset_retry(now)

    def observe_connection(self, connected: bool, now: float) -> None:
        connection_lost = not connected
        if self.connection_lost != connection_lost:
            self.connection_lost = connection_lost
            self.reset_retry(now)

    def reset_retry(self, now: float) -> None:
        self.retry_at = now
        self.attempts = 0

    def ready(self, now: float) -> bool:
        if now < max(self.settled_at, self.retry_at):
            return False
        if self.connection_lost:
            return True
        return self.observed != self.applied and self.attempts < MAX_SAVE_ATTEMPTS

    def succeeded(self, now: float) -> None:
        self.applied = self.observed
        if self.connection_lost:
            self.failed(now)
        else:
            self.reset_retry(now)

    def failed(self, now: float) -> None:
        self.retry_at = now + RETRY_SECONDS[self.attempts]
        self.attempts = min(self.attempts + 1, len(RETRY_SECONDS) - 1)


def _private_open(path: str, flags: int) -> int:
    return os.open(path, flags | os.O_NOFOLLOW, 0o600)


def _lease_path(state: Path, key: str) -> Path:
    return state / f"watch-{key}.lease"


@contextmanager
def _guard(state: Path, key: str) -> Iterator[None]:
    state.mkdir(mode=0o700, parents=True, exist_ok=True)
    with open(state / f"watch-{key}.lock", "a", opener=_private_open) as guard:
        fcntl.flock(guard, fcntl.LOCK_EX)
        yield


def current(state: Path, key: str, token: str) -> bool:
    try:
        path = _lease_path(state, key)
        with open(path, encoding="ascii", opener=_private_open) as lease:
            return lease.read() == token
    except FileNotFoundError:
        return False


def active(state: Path, key: str) -> bool:
    try:
        with open(_lease_path(state, key), "r+", opener=_private_open) as lease:
            try:
                fcntl.flock(lease, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                return current(state, key, lease.read())
    except FileNotFoundError:
        pass
    return False


def start(
    state: Path, root: Path, host: str, key: str, baseline: str, *, reconnect: bool
) -> None:
    state = state.absolute()
    root = root.resolve()
    with _guard(state, key):
        if active(state, key):
            return
        lease_path = _lease_path(state, key)
        token = uuid.uuid4().hex
        pending = state / f"watch-{key}-{token}.tmp"
        try:
            with open(pending, "x+", encoding="ascii", opener=_private_open) as lease:
                fcntl.flock(lease, fcntl.LOCK_EX)
                lease.write(token)
                lease.flush()
                pending.replace(lease_path)
                descriptor = lease.fileno()
                try:
                    worker = subprocess.Popen(
                        [
                            sys.executable,
                            "-m",
                            "remote_dev.watch",
                            str(state),
                            str(root),
                            host,
                            key,
                            baseline,
                            str(int(reconnect)),
                            str(descriptor),
                        ],
                        cwd=Path(__file__).resolve().parent.parent,
                        stdin=subprocess.DEVNULL,
                        stdout=subprocess.DEVNULL,
                        stderr=subprocess.DEVNULL,
                        pass_fds=(descriptor,),
                        start_new_session=True,
                    )
                except OSError:
                    lease_path.unlink(missing_ok=True)
                    raise
        finally:
            pending.unlink(missing_ok=True)
    threading.Thread(target=worker.wait, daemon=True).start()


def stop(state: Path, key: str) -> None:
    with _guard(state, key):
        _lease_path(state, key).unlink(missing_ok=True)


def _event_path(state: Path, key: str) -> Path:
    return state / f"watch-{key}.log"


def tail(state: Path, key: str) -> str:
    try:
        path = _event_path(state, key)
        with open(path, encoding="utf-8", opener=_private_open) as event:
            return event.read().strip()
    except FileNotFoundError:
        return ""


def _record(state: Path, key: str, token: str, message: str) -> None:
    with _guard(state, key):
        if not current(state, key, token):
            return
        path = _event_path(state, key)
        temporary = path.with_suffix(".event.tmp")
        text = "".join(
            character if character.isprintable() else " "
            for character in message[:MAX_EVENT_LENGTH]
        )
        date = datetime.now().isoformat(sep=" ", timespec="seconds")
        with open(
            temporary, "w", encoding="utf-8", errors="replace", opener=_private_open
        ) as event:
            event.write(f"{date} {text}\n")
        temporary.replace(path)


def _run(
    state: Path,
    root: Path,
    host: str,
    key: str,
    token: str,
    baseline: str,
    reconnect: bool,
) -> None:
    from .client import Client

    client = Client(quiet=True, state=state)
    schedule = SyncSchedule(baseline)
    next_connection_check = 0.0
    last_scan_error = None
    _record(state, key, token, "Watching for saved changes.")
    while current(state, key, token):
        time.sleep(POLL_SECONDS)
        if not current(state, key, token):
            return
        try:
            observed = fingerprint(root)
        except (OSError, ValueError) as error:
            message = str(error)
            if message != last_scan_error:
                _record(state, key, token, f"Source scan failed: {message}")
            last_scan_error = message
            continue
        if last_scan_error is not None:
            _record(state, key, token, "Source scan resumed.")
            last_scan_error = None
        now = time.monotonic()
        schedule.observe(observed, now)
        if reconnect and now >= next_connection_check:
            schedule.observe_connection(client.connected(key), now)
            next_connection_check = now + CONNECTION_CHECK_SECONDS
        if not schedule.ready(now) or not current(state, key, token):
            continue
        try:
            client.execute("sync", root, host_override=host, watch_token=token)
        except (OSError, ValueError, RuntimeError) as error:
            schedule.failed(time.monotonic())
            _record(state, key, token, f"Automatic sync failed: {error}")
        else:
            completed = time.monotonic()
            if schedule.connection_lost:
                schedule.observe_connection(client.connected(key), completed)
                next_connection_check = completed + CONNECTION_CHECK_SECONDS
            schedule.succeeded(completed)
            _record(state, key, token, "Automatic sync finished.")


def _main() -> None:
    state_path, root, host, key, baseline, reconnect, descriptor = sys.argv[1:]
    state = Path(state_path)
    with os.fdopen(int(descriptor), encoding="ascii") as lease:
        os.set_inheritable(lease.fileno(), False)
        lease.seek(0)
        token = lease.read()
        try:
            _run(state, Path(root), host, key, token, baseline, reconnect == "1")
        except (OSError, ValueError, RuntimeError) as error:
            _record(state, key, token, f"Watcher stopped: {error}")


if __name__ == "__main__":
    _main()

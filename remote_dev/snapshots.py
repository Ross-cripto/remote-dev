from __future__ import annotations

from contextlib import contextmanager
from dataclasses import dataclass
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import tarfile
import tempfile
from typing import Iterator


GENERATED_COMPOSE = ".remote-dev.generated.json"
PRIVATE_DIRECTORIES = {".git", ".ssh", ".aws", ".gnupg", "node_modules", "__pycache__"}
PRIVATE_FILES = {
    "remote-dev.toml",
    ".npmrc",
    ".netrc",
    ".pypirc",
    "id_rsa",
    "id_ed25519",
}
ENV_EXAMPLES = (".example", ".sample", ".template", ".dist")


@dataclass(frozen=True)
class Snapshot:
    path: Path
    digest: str


def git(root: Path, *arguments: str) -> bytes:
    result = subprocess.run(
        ["git", "-C", str(root), *arguments],
        capture_output=True,
        env=dict(os.environ, GIT_OPTIONAL_LOCKS="0"),
    )
    if result.returncode:
        raise ValueError(
            result.stderr.decode(errors="replace").strip() or "Git command failed"
        )
    return result.stdout


def find_worktree(directory: Path) -> Path:
    return Path(
        os.fsdecode(git(directory, "rev-parse", "--show-toplevel").rstrip(b"\n"))
    ).resolve()


def private_path(path: Path) -> bool:
    if PRIVATE_DIRECTORIES.intersection(path.parts) or path.name in PRIVATE_FILES:
        return True
    if path.name.endswith((".pem", ".key", ".p12", ".pfx")):
        return True
    is_environment_file = path.name == ".env" or path.name.startswith(".env.")
    return is_environment_file and not path.name.endswith(ENV_EXAMPLES)


def source_paths(root: Path) -> list[Path]:
    entries = git(root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
    paths = []
    for entry in sorted(set(entries.split(b"\0")) - {b""}):
        path = Path(os.fsdecode(entry))
        if path.is_absolute() or ".." in path.parts:
            raise ValueError(f"Unsafe Git path: {path}")
        if path.as_posix() == GENERATED_COMPOSE:
            raise ValueError(f"{GENERATED_COMPOSE} is reserved by Remote Dev")
        if not private_path(path):
            paths.append(path)
    return paths


def identity(file_info: os.stat_result) -> tuple[int, int, int, int, int]:
    return (
        file_info.st_dev,
        file_info.st_ino,
        file_info.st_size,
        file_info.st_mtime_ns,
        file_info.st_mode,
    )


def add_file(archive: tarfile.TarFile, root: Path, relative: Path) -> tuple | None:
    path = root / relative
    if not path.resolve().is_relative_to(root):
        raise ValueError(f"Source path points outside the worktree: {relative}")
    if path.resolve() != path:
        raise ValueError(f"Symlinks are unsupported in source snapshots: {relative}")
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except FileNotFoundError:
        return None
    with os.fdopen(descriptor, "rb") as source:
        before = os.fstat(source.fileno())
        if not stat.S_ISREG(before.st_mode):
            raise ValueError(f"Expected a regular source file: {relative}")
        member = tarfile.TarInfo(relative.as_posix())
        member.size = before.st_size
        member.mode = 0o755 if before.st_mode & stat.S_IXUSR else 0o644
        archive.addfile(member, source)
    if identity(before) != identity(path.lstat()):
        raise ValueError(
            f"Source changed while preparing the snapshot: {relative}. Retry."
        )
    return identity(before)


@contextmanager
def capture(root: Path, generated: dict | None) -> Iterator[Snapshot]:
    root = root.resolve()
    paths = source_paths(root)
    identities = {}
    with tempfile.TemporaryDirectory(prefix="remote-dev-source-") as directory:
        archive_path = Path(directory) / "source.tar.gz"
        with archive_path.open("wb") as raw:
            with gzip.GzipFile(fileobj=raw, mode="wb", mtime=0) as compressed:
                with tarfile.open(fileobj=compressed, mode="w|") as archive:
                    for relative in paths:
                        identities[relative] = add_file(archive, root, relative)
                    if generated is not None:
                        payload = json.dumps(generated, sort_keys=True).encode()
                        member = tarfile.TarInfo(GENERATED_COMPOSE)
                        member.size = len(payload)
                        archive.addfile(member, io.BytesIO(payload))
        if source_paths(root) != paths:
            raise ValueError(
                "Git file selection changed during snapshot capture. Retry."
            )
        for relative, captured in identities.items():
            try:
                current = identity((root / relative).lstat())
            except FileNotFoundError:
                current = None
            if current != captured:
                raise ValueError(
                    f"Source changed while preparing the snapshot: {relative}. Retry."
                )
        with archive_path.open("rb") as source:
            digest = hashlib.file_digest(source, "sha256").hexdigest()
        yield Snapshot(archive_path, digest)

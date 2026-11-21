from __future__ import annotations

from contextlib import contextmanager
import fcntl
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
from typing import BinaryIO, Iterator


LOOPBACK = "127.0.0.1"
COMPOSE_TIMEOUT_SECONDS = 180
LOG_LINES = 120
DELTA_METADATA = ".remote-dev.delta.json"


def file_digest(path: Path) -> str:
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def path_in(root: Path, value: str) -> Path:
    relative = PurePosixPath(value)
    if (
        relative.is_absolute()
        or not relative.parts
        or {"..", ".git"}.intersection(relative.parts)
    ):
        raise ValueError(f"Unsafe workspace path: {value}")
    candidate = root.resolve() / relative
    if candidate.resolve() != candidate:
        raise ValueError(f"Unexpected symlink in workspace path: {value}")
    return candidate


def source_path(workspace: Path, digest: str) -> Path:
    if not re.fullmatch(r"[0-9a-f]{64}", digest):
        raise ValueError("Invalid snapshot digest")
    return workspace / "snapshots" / digest / "source"


def inventory(root: Path) -> dict[str, str]:
    records = {}
    for entry in root.rglob("*"):
        if entry.is_symlink():
            raise ValueError(
                f"Remote snapshot changed: symlink {entry.relative_to(root)}"
            )
        if entry.is_file():
            records[entry.relative_to(root).as_posix()] = file_digest(entry)
        elif not entry.is_dir():
            raise ValueError(
                f"Remote snapshot changed: unsupported entry {entry.relative_to(root)}"
            )
    return records


def verify_snapshot(workspace: Path, digest: str) -> Path:
    source = source_path(workspace, digest)
    manifest = source.parent / "files.json"
    if not source.is_dir() or not manifest.is_file():
        raise ValueError("Remote snapshot is incomplete. Run remote-dev up to retry.")
    if inventory(source) != json.loads(manifest.read_text()):
        raise ValueError(
            "Remote source changed. Preserve those changes before syncing this workspace."
        )
    return source


@contextmanager
def verified_archive(
    source: Path, digest: str, data: BinaryIO
) -> Iterator[tuple[tarfile.TarFile, Path]]:
    source.parent.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(
        prefix="incoming-", dir=source.parent.parent
    ) as directory:
        incoming = Path(directory)
        payload = incoming / "source.tar"
        with payload.open("wb") as output:
            shutil.copyfileobj(data, output)
        if file_digest(payload) != digest:
            raise ValueError(
                "Snapshot transfer failed its digest check. Retry the operation."
            )
        with tarfile.open(payload, mode="r:*") as archive:
            yield archive, incoming / "snapshot" / "source"


def extract_snapshot_member(
    archive: tarfile.TarFile, member: tarfile.TarInfo, root: Path, *, replace: bool
) -> None:
    target = path_in(root, member.name)
    if not member.isfile():
        raise ValueError(f"Unsupported snapshot member: {member.name}")
    target.parent.mkdir(parents=True, exist_ok=True)
    if replace:
        target.unlink(missing_ok=True)
    with archive.extractfile(member) as content, target.open("xb") as output:
        shutil.copyfileobj(content, output)
    target.chmod(0o755 if member.mode & 0o100 else 0o644)


def publish_snapshot(staged: Path, source: Path) -> None:
    for directory in staged.rglob("*"):
        if directory.is_dir():
            directory.chmod(0o755)
    (staged.parent / "files.json").write_text(
        json.dumps(inventory(staged), sort_keys=True)
    )
    staged.parent.rename(source.parent)


def install_snapshot(workspace: Path, digest: str, data: BinaryIO) -> None:
    source = source_path(workspace, digest)
    if source.parent.exists():
        verify_snapshot(workspace, digest)
        return
    with verified_archive(source, digest, data) as (archive, staged):
        staged.mkdir(parents=True)
        staged.chmod(0o755)
        for member in archive:
            extract_snapshot_member(archive, member, staged, replace=False)
        publish_snapshot(staged, source)


def delta_description(archive: tarfile.TarFile) -> tuple[str, list[str]]:
    member = archive.next()
    if member is None or member.name != DELTA_METADATA or not member.isfile():
        raise ValueError("The snapshot delta is missing its manifest.")
    with archive.extractfile(member) as content:
        manifest = json.load(content)
    base = manifest.get("base") if isinstance(manifest, dict) else None
    deleted = manifest.get("deleted") if isinstance(manifest, dict) else None
    if not isinstance(base, str) or not isinstance(deleted, list):
        raise ValueError("Invalid snapshot delta manifest.")
    if not all(isinstance(name, str) for name in deleted):
        raise ValueError("Invalid snapshot delta deletions.")
    return base, deleted


def remove_snapshot_file(root: Path, name: str) -> None:
    target = path_in(root, name)
    target.unlink()
    for parent in target.parents:
        if parent == root or any(parent.iterdir()):
            break
        parent.rmdir()


def install_delta(
    workspace: Path, digest: str, deployed: str | None, data: BinaryIO
) -> bool:
    source = source_path(workspace, digest)
    with verified_archive(source, digest, data) as (archive, staged):
        base, deleted = delta_description(archive)
        source_path(workspace, base)
        if digest == deployed or base == deployed and source.parent.exists():
            verify_snapshot(workspace, digest)
            return True
        if base != deployed:
            return False
        shutil.copytree(verify_snapshot(workspace, base), staged)
        for name in deleted:
            remove_snapshot_file(staged, name)
        seen = {DELTA_METADATA}
        while (member := archive.next()) is not None:
            if member.name in seen:
                raise ValueError(f"Duplicate snapshot member: {member.name}")
            seen.add(member.name)
            extract_snapshot_member(archive, member, staged, replace=True)
        publish_snapshot(staged, source)
    return True


def run(arguments: list[str], cwd: Path | None = None, progress: bool = False) -> str:
    environment = {
        key: value
        for key, value in os.environ.items()
        if not key.startswith("COMPOSE_")
    }
    result = subprocess.run(
        arguments,
        cwd=cwd,
        env=environment,
        text=True,
        stdout=sys.stderr if progress else subprocess.PIPE,
        stderr=None if progress else subprocess.PIPE,
    )
    if result.returncode:
        detail = result.stderr.strip() if result.stderr else f"exit {result.returncode}"
        raise ValueError(f"{' '.join(arguments[:2])} failed: {detail}")
    return result.stdout or ""


def validate_runtime(configuration: dict, forwards: list[dict]) -> None:
    services = configuration.get("services", {})
    for name, service in services.items():
        if service.get("network_mode") == "host" or service.get("container_name"):
            raise ValueError(
                f"Service {name} must use project networking and a Compose-generated container name"
            )
        for binding in service.get("ports", []):
            if binding.get("host_ip") != LOOPBACK:
                raise ValueError(
                    f"Bind every published port for {name} to {LOOPBACK} in Compose"
                )
    for forward in forwards:
        ports = services.get(forward["service"], {}).get("ports", [])
        matches = [
            binding
            for binding in ports
            if binding.get("target") == forward["target"]
            and binding.get("protocol", "tcp") == "tcp"
        ]
        if len(matches) != 1:
            raise ValueError(
                f"Publish exactly one TCP port {forward['target']} on service {forward['service']}"
            )


@contextmanager
def workspace_lock(workspace: Path) -> Iterator[None]:
    workspace.mkdir(mode=0o700, parents=True, exist_ok=True)
    with (workspace / "operation.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        yield


def selected_containers(project: str, *, running_only: bool) -> list[str]:
    command = [
        "docker",
        "ps",
        "--quiet",
        "--filter",
        "label=com.docker.compose.project=" + project,
    ]
    if not running_only:
        command.append("--all")
    return run(command).split()


def compose_command(
    project: str, source: Path, compose_file: str
) -> tuple[list[str], Path]:
    path = path_in(source, compose_file)
    if not path.is_file():
        raise ValueError(f"Compose file missing from source snapshot: {compose_file}")
    return [
        "docker",
        "compose",
        "--project-name",
        project,
        "--file",
        str(path),
    ], path.parent


def inspect_ports(
    command: list[str], cwd: Path, forwards: list[dict]
) -> dict[str, int]:
    result = {}
    for forward in forwards:
        binding = run(
            [*command, "port", forward["service"], str(forward["target"])], cwd
        ).strip()
        match = re.fullmatch(r"127\.0\.0\.1:([0-9]+)", binding)
        if match is None or not 1 <= int(match[1]) <= 65535:
            raise ValueError(f"No loopback binding for {forward['name']}")
        result[forward["name"]] = int(match[1])
    return result


INFRASTRUCTURE_SERVICES = {
    "database",
    "db",
    "postgres",
    "postgresql",
    "mysql",
    "mariadb",
    "redis",
    "valkey",
    "mongo",
    "mongodb",
    "rabbitmq",
    "memcached",
    "elasticsearch",
}
DEPENDENCY_INPUTS = {
    "package.json",
    "package-lock.json",
    "npm-shrinkwrap.json",
    "pnpm-lock.yaml",
    "pnpm-workspace.yaml",
    "yarn.lock",
    "bun.lock",
    "bun.lockb",
    "Dockerfile",
    "requirements.txt",
    "pyproject.toml",
    "uv.lock",
}
LIVE_SOURCE_LABEL = "io.remote-dev.live-source"
RELOAD_LABEL = "io.remote-dev.reload"


def exported_ports(service: dict) -> set[int]:
    targets = set()
    for binding in service.get("ports", []):
        target = binding.get("target")
        if binding.get("protocol", "tcp") == "tcp" and type(target) is int:
            targets.add(target)
    for value in service.get("expose", []):
        match = re.fullmatch(r"([0-9]+)(?:/tcp)?", str(value))
        if match:
            targets.add(int(match[1]))
    return {port for port in targets if 1 <= port <= 65535}


def configure_automatic_ports(configuration: dict, declared: list[dict]) -> list[dict]:
    services = configuration.get("services", {})
    forwards = list(declared)
    if not declared:
        for name, service in services.items():
            image = service.get("image", "").partition(":")[0].rsplit("/", 1)[-1]
            if (
                name.casefold() in INFRASTRUCTURE_SERVICES
                or image in INFRASTRUCTURE_SERVICES
            ):
                continue
            ports = sorted(exported_ports(service))
            for port in ports:
                label = name if len(ports) == 1 else f"{name}-{port}"
                forwards.append({"name": label, "service": name, "target": port})
    for service in services.values():
        service.pop("ports", None)
    for forward in forwards:
        service = services.get(forward["service"])
        if service is None:
            raise ValueError(f"Missing service for forward {forward['name']}")
        binding = {"host_ip": LOOPBACK, "target": forward["target"], "protocol": "tcp"}
        bindings = service.setdefault("ports", [])
        if binding not in bindings:
            bindings.append(binding)
    validate_runtime(configuration, forwards)
    return forwards


def read_json(path: Path) -> dict:
    if not path.exists():
        return {}
    return json.loads(path.read_text())


def write_json(path: Path, content: dict) -> None:
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(content, sort_keys=True))
    temporary.replace(path)


def runtime_source(workspace: Path) -> Path:
    return workspace / "runtime-source"


def apply_runtime_source(
    workspace: Path, previous: Path | None, desired: Path
) -> tuple[list[str], list[str]]:
    runtime = runtime_source(workspace)
    if runtime.is_symlink():
        raise ValueError("Unexpected symlink in the runtime source.")
    runtime = runtime.resolve()
    runtime.mkdir(mode=0o755, exist_ok=True)
    runtime.chmod(0o755)
    before = read_json(previous.parent / "files.json") if previous else {}
    after = read_json(desired.parent / "files.json")
    current = inventory(runtime)
    for name in before.keys() | after.keys() | current.keys():
        if current.get(name) not in (before.get(name), after.get(name)):
            raise ValueError(
                f"Remote source changed at {name}. Preserve that edit before syncing."
            )
    deleted = sorted(before.keys() - after.keys())
    changed = sorted(name for name, value in after.items() if before.get(name) != value)
    with tempfile.TemporaryDirectory(
        prefix="source-update-", dir=workspace
    ) as directory:
        staged = Path(directory)
        for name in changed:
            incoming = path_in(staged, name)
            incoming.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(path_in(desired, name), incoming)
        for name in deleted:
            if name in current:
                target = path_in(runtime, name)
                target.unlink()
        for name in changed:
            target = path_in(runtime, name)
            if target.is_dir():
                for directory, _, _ in os.walk(target, topdown=False):
                    if Path(directory) != target:
                        Path(directory).rmdir()
                target.rmdir()
            target.parent.mkdir(parents=True, exist_ok=True)
            for parent in target.parents:
                if parent == runtime:
                    break
                parent.chmod(0o755)
            path_in(staged, name).replace(target)
    if inventory(runtime) != after:
        raise ValueError(
            "Runtime source did not match the prepared revision. Retry sync."
        )
    return changed, deleted


def source_compose_command(
    project: str, source: Path, request: dict
) -> tuple[list[str], Path]:
    command, cwd = compose_command(project, source, request["compose_file"])
    override = request.get("compose_override")
    if override:
        command.extend(["--file", str(path_in(source, override))])
    overlay = ".remote-dev.generated.json"
    if request["compose_file"] != overlay and (source / overlay).is_file():
        command.extend(["--file", str(path_in(source, overlay))])
    profiles = request.get("compose_profiles", [])
    if request.get("auto_ports") and not profiles:
        available = set(run([*command, "config", "--profiles"], cwd).splitlines())
        profiles = sorted(available.intersection({"dev", "development"}))
    for profile in profiles:
        command.extend(["--profile", profile])
    return command, cwd


def escaped_compose_value(value):
    if isinstance(value, str):
        return value.replace("$", "$$")
    if isinstance(value, list):
        return [escaped_compose_value(item) for item in value]
    if isinstance(value, dict):
        return {key: escaped_compose_value(item) for key, item in value.items()}
    return value


def runtime_compose_command(workspace: Path, project: str, source: Path) -> list[str]:
    return [
        "docker",
        "compose",
        "--project-name",
        project,
        "--project-directory",
        str(source),
        "--file",
        str(workspace / "runtime-compose.json"),
    ]


def running_services(project: str) -> dict[str, list[str]]:
    rows = run(
        [
            "docker",
            "ps",
            "--filter",
            "label=com.docker.compose.project=" + project,
            "--format",
            '{{.ID}}\t{{.Label "com.docker.compose.service"}}',
        ]
    )
    services = {}
    for row in rows.splitlines():
        container, _, service = row.partition("\t")
        services.setdefault(service, []).append(container)
    return services


def update_node_containers(
    source: Path,
    configuration: dict,
    containers: dict[str, list[str]],
    changed: list[str],
    deleted: list[str],
) -> None:
    with tempfile.TemporaryFile() as archive_stream:
        with tarfile.open(fileobj=archive_stream, mode="w") as archive:
            for name in changed:
                archive.add(path_in(source, name), arcname=name, recursive=False)
        for service_name, service in configuration["services"].items():
            labels = service.get("labels", {})
            if (
                labels.get(LIVE_SOURCE_LABEL) != "/workspace"
                or labels.get(RELOAD_LABEL) != "watch"
            ):
                continue
            for container in containers.get(service_name, []):
                for relative in deleted:
                    run(
                        [
                            "docker",
                            "exec",
                            container,
                            "rm",
                            "-f",
                            "--",
                            "/workspace/" + relative,
                        ]
                    )
                if changed:
                    archive_stream.seek(0)
                    result = subprocess.run(
                        ["docker", "cp", "-", container + ":/workspace"],
                        stdin=archive_stream,
                        stdout=sys.stderr,
                    )
                    if result.returncode:
                        raise ValueError(
                            f"Could not update source for {service_name}. Retry sync."
                        )


def compose_up(
    command: list[str],
    cwd: Path,
    *,
    rebuild: bool,
    recreate: bool,
    services: list[str] | None = None,
) -> None:
    arguments = [
        *command,
        "up",
        "--detach",
        "--wait",
        "--wait-timeout",
        str(COMPOSE_TIMEOUT_SECONDS),
    ]
    if services:
        arguments.append("--no-deps")
    else:
        arguments.append("--remove-orphans")
    if rebuild:
        arguments.append("--build")
    if recreate:
        arguments.append("--force-recreate")
    if services:
        arguments.extend(services)
    run(arguments, cwd, progress=True)


def uses_build(configuration: dict) -> bool:
    return any("build" in service for service in configuration["services"].values())


def workspace_binds(source: Path, service: dict) -> list[Path]:
    paths = []
    for volume in service.get("volumes", []):
        if volume.get("type") == "bind" and "source" in volume:
            path = Path(volume["source"])
            if path.is_relative_to(source):
                paths.append(path)
    return paths


def reads_files_on_request(service: dict) -> bool:
    command = service.get("command") or []
    image = service.get("image", "").rsplit("/", 1)[-1].partition(":")[0]
    return "http.server" in command or image in {"nginx", "httpd", "caddy"}


def services_to_reload(
    source: Path, configuration: dict, containers: dict, changes: set[str]
) -> list[str]:
    refresh = []
    for name, service in configuration["services"].items():
        reload_mode = service.get("labels", {}).get(RELOAD_LABEL)
        if (
            name not in containers
            or reload_mode == "watch"
            or reads_files_on_request(service)
        ):
            continue
        mounts = workspace_binds(source, service)
        if any(
            (source / change).is_relative_to(mount)
            for change in changes
            for mount in mounts
        ):
            refresh.append(name)
    return refresh


def refresh_services(
    command: list[str],
    source: Path,
    configuration: dict,
    containers: dict[str, list[str]],
    changed: list[str],
    deleted: list[str],
) -> None:
    modified = set(changed) | set(deleted)
    if not modified:
        return
    if uses_build(configuration):
        compose_up(command, source, rebuild=True, recreate=False)
        return
    generated = [
        name
        for name, service in configuration["services"].items()
        if service.get("labels", {}).get(LIVE_SOURCE_LABEL) == "/workspace"
    ]
    if generated and any(
        PurePosixPath(name).name in DEPENDENCY_INPUTS for name in modified
    ):
        compose_up(command, source, rebuild=False, recreate=True, services=generated)
        return
    modified_paths = {source / name for name in modified}
    remount = [
        name
        for name, service in configuration["services"].items()
        if name in containers
        and modified_paths.intersection(workspace_binds(source, service))
    ]
    if remount:
        compose_up(command, source, rebuild=False, recreate=True, services=remount)
        containers = {
            name: ids for name, ids in containers.items() if name not in remount
        }
    update_node_containers(source, configuration, containers, changed, deleted)
    restart = services_to_reload(source, configuration, containers, modified)
    if restart:
        run([*command, "restart", *restart], source, progress=True)


def activate(workspace: Path, project: str, action: str, request: dict) -> dict:
    manifest = workspace / "deployment.json"
    journal = workspace / "source-update.json"
    previous = read_json(manifest)
    interrupted = read_json(journal)
    previous_source = (
        verify_snapshot(workspace, previous["digest"]) if previous else None
    )
    digest = request["digest"]
    if request.get("delta"):
        if not install_delta(
            workspace, digest, previous.get("digest"), sys.stdin.buffer
        ):
            return {"need_full": True}
    else:
        install_snapshot(workspace, digest, sys.stdin.buffer)
    desired = verify_snapshot(workspace, digest)
    source_command, cwd = source_compose_command(project, desired, request)
    preview = json.loads(run([*source_command, "config", "--format", "json"], cwd))
    forwards = request["forwards"]
    if request.get("auto_ports"):
        forwards = configure_automatic_ports(preview, forwards)
    else:
        validate_runtime(preview, forwards)

    baseline = previous_source if previous.get("runtime_source") else None
    source = runtime_source(workspace).resolve()
    if interrupted:
        starting = interrupted.get("previous")
        interrupted_baseline = (
            verify_snapshot(workspace, starting) if starting else None
        )
        interrupted_source = verify_snapshot(workspace, interrupted["desired"])
        apply_runtime_source(workspace, interrupted_baseline, interrupted_source)
        baseline = interrupted_source
    expected = read_json(baseline.parent / "files.json") if baseline else {}
    if source.exists() and inventory(source) != expected:
        raise ValueError("Remote source changed. Preserve that edit before syncing.")
    write_json(
        journal,
        {"previous": baseline.parent.name if baseline else None, "desired": digest},
    )
    changed, deleted = apply_runtime_source(workspace, baseline, desired)
    source_command, cwd = source_compose_command(project, source, request)
    configuration = json.loads(
        run([*source_command, "config", "--format", "json"], cwd)
    )
    if request.get("auto_ports"):
        configure_automatic_ports(configuration, forwards)
    else:
        validate_runtime(configuration, forwards)
    for service in configuration["services"].values():
        service.pop("profiles", None)
    runtime_file = workspace / "runtime-compose.json"
    resolved = escaped_compose_value(configuration)
    config_changed = read_json(runtime_file) != resolved
    if config_changed:
        write_json(runtime_file, resolved)
    command = runtime_compose_command(workspace, project, source)
    containers = running_services(project)
    running = action == "up" or bool(containers)
    if running:
        if action == "up" or config_changed or interrupted:
            compose_up(
                command,
                source,
                rebuild=uses_build(configuration),
                recreate=bool(interrupted),
            )
            if not config_changed and not interrupted:
                refresh_services(
                    command,
                    source,
                    configuration,
                    running_services(project),
                    changed,
                    deleted,
                )
        else:
            refresh_services(
                command, source, configuration, containers, changed, deleted
            )
    ports = inspect_ports(command, source, forwards) if running else {}
    write_json(
        manifest,
        {
            "digest": digest,
            "compose_file": request["compose_file"],
            "runtime_source": True,
            "forwards": forwards,
        },
    )
    journal.unlink()
    if previous.get("runtime_source") and previous_source != desired:
        shutil.rmtree(previous_source.parent)
    if interrupted and interrupted_source not in (previous_source, desired):
        shutil.rmtree(interrupted_source.parent)
    return {"running": running, "ports": ports}


def host_operation(action: str, key: str, request: dict) -> str:
    if action == "doctor":
        engine = run(["docker", "info", "--format", "{{.OSType}}"]).strip()
        if engine != "linux":
            raise ValueError("Expected a Linux Docker engine")
        return f"Python {sys.version.split()[0]} · {run(['docker', 'compose', 'version']).strip()}"
    if not re.fullmatch(r"[0-9a-f]{24}", key):
        raise ValueError("Invalid workspace identity")
    data_dir = Path(os.environ.get("XDG_DATA_HOME") or Path.home() / ".local/share")
    workspace = data_dir / "remote-dev/standalone-v1" / key
    project = "rd-" + key
    if action in {"up", "sync"}:
        with workspace_lock(workspace):
            return json.dumps(activate(workspace, project, action, request))
    if not workspace.is_dir():
        return "Workspace has not run yet. Use remote-dev up."
    with workspace_lock(workspace):
        if action == "stop":
            containers = selected_containers(project, running_only=True)
            if containers:
                run(["docker", "stop", *containers], progress=True)
            return "Workspace stopped."
        if action == "status":
            return run(
                [
                    "docker",
                    "ps",
                    "--all",
                    "--filter",
                    "label=com.docker.compose.project=" + project,
                    "--format",
                    'table {{.Label "com.docker.compose.service"}}\t{{.Status}}',
                ]
            ).strip()
        if action == "logs":
            manifest = json.loads((workspace / "deployment.json").read_text())
            if (
                manifest.get("runtime_source")
                or (workspace / "runtime-compose.json").is_file()
            ):
                cwd = runtime_source(workspace)
                command = runtime_compose_command(workspace, project, cwd)
            else:
                command, cwd = compose_command(
                    project,
                    source_path(workspace, manifest["digest"]),
                    manifest["compose_file"],
                )
            return run(
                [*command, "logs", "--no-color", "--tail", str(LOG_LINES)], cwd
            ).strip()
    raise ValueError(f"Unknown host operation: {action}")


if __name__ == "__main__":
    os.umask(0o077)
    try:
        print(host_operation(sys.argv[1], sys.argv[2], json.loads(sys.argv[3])))
    except (OSError, ValueError, KeyError, tarfile.TarError) as error:
        print(f"remote-dev: {error}", file=sys.stderr)
        raise SystemExit(1)

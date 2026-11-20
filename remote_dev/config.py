import json
import re
import shlex
import tomllib
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import Any

from .connection import Connection

CONFIG_FILE = "remote-dev.toml"
DEFAULT_COMPOSE_FILE = "compose.yaml"
GENERATED_COMPOSE_FILE = ".remote-dev.generated.json"
REMOTE_LOOPBACK = "127.0.0.1"
WORKSPACE = PurePosixPath("/workspace")
NODE_SOURCE = PurePosixPath("/remote-source")
NODE_IMAGE = "node:24-bookworm-slim"
LIVE_SOURCE_LABEL = "io.remote-dev.live-source"
RELOAD_LABEL = "io.remote-dev.reload"
MAX_TCP_PORT = 65535
NAME_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]*")
ENVIRONMENT_NAME_PATTERN = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")
SHELL_ASSIGNMENT = re.compile(r"([A-Za-z_][A-Za-z0-9_]*)=(.*)")
MODE_TEMPLATES = {
    "compose": """
# Each forwarded service must publish its target port on 127.0.0.1 in Compose.
compose_file = "compose.yaml"

# [forwards.web]
# service = "web"
# target = 8000
# local = "auto"
""",
    "commands": """
# Commands run inside the selected image with the synced project mounted at /workspace.
[services.web]
image = "python:3.13-slim"
command = ["python", "-m", "http.server", "8000", "--bind", "0.0.0.0"]

[forwards.web]
service = "web"
target = 8000
local = "auto"
""",
    "node": """
# Each directory needs a package.json with the chosen npm script.
# The script must listen on 0.0.0.0 and the matching target port inside Docker.
[services.frontend]
directory = "frontend"
script = "dev"

[services.backend]
directory = "backend"
script = "dev"

[forwards.frontend]
service = "frontend"
target = 3000
local = "auto"

[forwards.backend]
service = "backend"
target = 4000
local = "auto"
""",
}


class ConfigError(ValueError):
    pass


@dataclass(frozen=True)
class Forward:
    name: str
    service: str
    target: int
    local: int | None


@dataclass(frozen=True)
class Project:
    host: str
    mode: str
    compose_file: str
    generated_compose: dict[str, Any] | None
    forwards: tuple[Forward, ...]
    auto_ports: bool = False
    detected_from: str | None = None
    compose_override: str | None = None
    compose_profiles: tuple[str, ...] = ()


def init_project(root: Path, host: str, mode: str) -> Path:
    host = _validate_name(host, "SSH host alias")
    mode = _validate_mode(mode)
    config_path = root / CONFIG_FILE
    content = f'host = "{host}"\nmode = "{mode}"\n{MODE_TEMPLATES[mode]}'
    with config_path.open("x", encoding="utf-8") as config_file:
        config_file.write(content)
    return config_path


def load_project(root: Path, host_override: str | None = None) -> Project:
    config_path = root / CONFIG_FILE
    try:
        with config_path.open("rb") as config_file:
            config = tomllib.load(config_file)
    except (OSError, ValueError) as error:
        raise ConfigError(f"Cannot read {config_path}: {error}") from error

    if host_override is None:
        host = _validate_name(config.get("host"), "SSH host alias")
    else:
        Connection(host_override)
        host = host_override
    mode = _validate_mode(config.get("mode"))
    mode_key = "compose_file" if mode == "compose" else "services"
    _validate_keys(config, {"host", "mode", "forwards", mode_key}, CONFIG_FILE)
    forwards = _parse_forwards(config.get("forwards", {}))

    if mode == "compose":
        compose_file = _relative_path(
            root, config.get("compose_file", DEFAULT_COMPOSE_FILE), "compose_file"
        )
        if compose_file.as_posix() == GENERATED_COMPOSE_FILE:
            raise ConfigError(f"The compose_file {GENERATED_COMPOSE_FILE} is reserved")
        if not (root / compose_file).is_file():
            raise ConfigError(f"Compose file does not exist: {compose_file}")
        return Project(host, mode, compose_file.as_posix(), None, forwards)

    generated_compose = _generate_compose(root, mode, config.get("services"), forwards)
    return Project(host, mode, GENERATED_COMPOSE_FILE, generated_compose, forwards)


def _validate_mode(value: object) -> str:
    if not isinstance(value, str) or value not in MODE_TEMPLATES:
        raise ConfigError("mode must be compose, commands or node")
    return value


def _validate_name(value: object, key: str) -> str:
    if not isinstance(value, str) or not NAME_PATTERN.fullmatch(value):
        raise ConfigError(
            f"{key} must start with a letter or number and contain only "
            "letters, numbers, dots, underscores or hyphens"
        )
    return value


def _validate_keys(table: dict[str, Any], allowed: set[str], key: str) -> None:
    unexpected = table.keys() - allowed
    if unexpected:
        raise ConfigError(f"Unknown key in {key}: {', '.join(sorted(unexpected))}")


def _table(value: object, key: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ConfigError(f"{key} must be a table")
    return value


def _nonempty_string(value: object, key: str) -> str:
    if not isinstance(value, str) or not value.strip() or "\0" in value:
        raise ConfigError(f"{key} must be a non-empty string without null characters")
    return value


def _relative_path(root: Path, value: object, key: str) -> PurePosixPath:
    value = _nonempty_string(value, key)
    path = PurePosixPath(value)
    if path.is_absolute() or ".." in path.parts or not value.isprintable():
        raise ConfigError(
            f"{key} must be a project-relative path without parent traversal"
        )
    try:
        inside_project = (root / path).resolve().is_relative_to(root.resolve())
    except (OSError, RuntimeError) as error:
        raise ConfigError(f"Cannot resolve {key}: {value}") from error
    if not inside_project:
        raise ConfigError(f"{key} must point inside the project")
    return path


def _port(value: object, key: str) -> int:
    if type(value) is not int or not 1 <= value <= MAX_TCP_PORT:
        raise ConfigError(f"{key} must be an integer from 1 to {MAX_TCP_PORT}")
    return value


def _parse_forwards(value: object) -> tuple[Forward, ...]:
    forwards = []
    local_ports = set()
    for name, config in _table(value, "forwards").items():
        key = f"forwards.{name}"
        _validate_name(name, "Forward name")
        config = _table(config, key)
        _validate_keys(config, {"service", "target", "local"}, key)
        service = _validate_name(config.get("service"), f"{key}.service")
        target = _port(config.get("target"), f"{key}.target")
        local_port = config.get("local", "auto")
        local = None
        if local_port != "auto":
            local = _port(local_port, f"{key}.local")
            if local in local_ports:
                raise ConfigError(
                    f"Local port {local} is used by more than one forward"
                )
            local_ports.add(local)
        forwards.append(Forward(name, service, target, local))
    return tuple(forwards)


def _generate_compose(
    root: Path, mode: str, value: object, forwards: tuple[Forward, ...]
) -> dict[str, Any]:
    service_configs = _table(value, "services")
    if not service_configs:
        raise ConfigError(f"Configure at least one service for {mode} mode")

    targets_by_service: dict[str, set[int]] = {name: set() for name in service_configs}
    for forward in forwards:
        if forward.service not in targets_by_service:
            raise ConfigError(
                f"Forward {forward.name} references unknown service {forward.service}"
            )
        targets_by_service[forward.service].add(forward.target)

    services: dict[str, Any] = {}
    volumes: dict[str, Any] = {}
    mode_keys = {"image", "command"} if mode == "commands" else {"script", "install"}
    source_mount = WORKSPACE if mode == "commands" else NODE_SOURCE
    for name, config in service_configs.items():
        _validate_name(name, "Service name")
        key = f"services.{name}"
        config = _table(config, key)
        _validate_keys(config, {"directory", "environment"} | mode_keys, key)
        directory = _relative_path(
            root, config.get("directory", "."), f"{key}.directory"
        )
        if not (root / directory).is_dir():
            raise ConfigError(f"Service directory does not exist: {directory}")

        targets = sorted(targets_by_service[name])
        environment = _environment(config.get("environment", {}), f"{key}.environment")
        service = _base_service(directory, source_mount, environment, targets)
        if mode == "commands":
            image = _nonempty_string(config.get("image"), f"{key}.image")
            service["image"] = _escape_for_compose(image)
            service["command"] = _service_command(
                config.get("command"), f"{key}.command"
            )
        else:
            _configure_node(
                root / directory, name, directory, config, service, targets, volumes
            )
        services[name] = service

    compose = {"services": services}
    if volumes:
        compose["volumes"] = volumes
    return compose


def _base_service(
    directory: PurePosixPath,
    source_mount: PurePosixPath,
    environment: dict[str, str],
    targets: list[int],
) -> dict[str, Any]:
    service: dict[str, Any] = {
        "init": True,
        "working_dir": _escape_for_compose(str(WORKSPACE / directory)),
        "volumes": [
            {
                "type": "bind",
                "source": ".",
                "target": str(source_mount),
                "read_only": True,
            },
        ],
    }
    if environment:
        service["environment"] = environment
    if targets:
        service["ports"] = [
            {"target": target, "protocol": "tcp", "host_ip": REMOTE_LOOPBACK}
            for target in targets
        ]
    return service


def _environment(value: object, key: str) -> dict[str, str]:
    environment = {}
    for name, setting in _table(value, key).items():
        if not ENVIRONMENT_NAME_PATTERN.fullmatch(name):
            raise ConfigError(f"{key}.{name} must be a valid environment variable name")
        if not isinstance(setting, str) or "\0" in setting:
            raise ConfigError(f"{key}.{name} must be a string without null characters")
        environment[name] = _escape_for_compose(setting)
    return environment


def _escape_for_compose(value: str) -> str:
    return value.replace("$", "$$")


def _service_command(value: object, key: str) -> list[str]:
    if isinstance(value, str):
        return ["/bin/sh", "-c", _escape_for_compose(_nonempty_string(value, key))]
    if isinstance(value, list) and value:
        _nonempty_string(value[0], f"{key}[0]")
        for argument in value:
            if not isinstance(argument, str) or "\0" in argument:
                raise ConfigError(
                    f"{key} must contain only strings without null characters"
                )
        return [_escape_for_compose(argument) for argument in value]
    raise ConfigError(f"{key} must be shell text or a non-empty list of arguments")


def _configure_node(
    package_root: Path,
    name: str,
    directory: PurePosixPath,
    config: dict[str, Any],
    service: dict[str, Any],
    targets: list[int],
    volumes: dict[str, Any],
) -> None:
    key = f"services.{name}"
    if not (package_root / "package.json").is_file():
        raise ConfigError(f"{key}.directory must contain a package.json: {directory}")

    environment = service.setdefault("environment", {})
    environment.setdefault("HOST", "0.0.0.0")
    if len(targets) == 1:
        environment.setdefault("PORT", str(targets[0]))
    service["image"] = NODE_IMAGE
    script = _nonempty_string(config.get("script", "dev"), f"{key}.script")
    service["labels"] = node_labels(_manifest_script_watches(package_root, script))
    installer = _npm_installer(
        package_root, config.get("install", "auto"), f"{key}.install"
    )
    startup_steps = [f"cp -a {NODE_SOURCE}/. {WORKSPACE}"]
    if installer is not None:
        startup_steps.append(installer)
    startup_steps.append('exec npm run -- "$$1"')
    service["command"] = [
        "/bin/sh",
        "-c",
        " && ".join(startup_steps),
        "remote-dev",
        _escape_for_compose(script),
    ]
    if installer is None:
        return
    volume_name = f"{name}-node-modules"
    volumes[volume_name] = {}
    service["volumes"].append(
        {
            "type": "volume",
            "source": volume_name,
            "target": _escape_for_compose(str(WORKSPACE / directory / "node_modules")),
        }
    )


def _npm_installer(package_root: Path, install: object, key: str) -> str | None:
    if install == "none":
        return None
    has_lockfile = any(
        (package_root / filename).is_file()
        for filename in ("package-lock.json", "npm-shrinkwrap.json")
    )
    if install == "ci" and not has_lockfile:
        raise ConfigError(
            f"{key} requires a package-lock.json or npm-shrinkwrap.json when set to ci"
        )
    if install == "ci" or (install == "auto" and has_lockfile):
        return "npm ci"
    if install in ("auto", "install"):
        return "npm install --no-package-lock"
    raise ConfigError(f"{key} must be auto, none, ci or install")


def node_labels(watches: bool) -> dict[str, str]:
    return {
        LIVE_SOURCE_LABEL: str(WORKSPACE),
        RELOAD_LABEL: "watch" if watches else "restart",
    }


def node_command_watches(arguments: list[str]) -> bool:
    if not arguments:
        return False
    command, *options = arguments
    if command == "nodemon":
        return True
    if command == "vite":
        return starts_vite_dev_server(options)
    if command in {"next", "tsx"}:
        watch_command = "dev" if command == "next" else "watch"
        return options[:1] == [watch_command]
    if command == "nest":
        return options[:1] == ["start"] and bool(
            {"--watch", "-w"}.intersection(options)
        )
    return command == "node" and "--watch" in options


def starts_vite_dev_server(options: list[str]) -> bool:
    return not options or options[0].startswith("-") or options[0] in {"dev", "serve"}


def parse_dev_command(script: str) -> tuple[list[str], dict[str, str]]:
    if "\n" in script:
        return [], {}
    lexer = shlex.shlex(script, posix=True, punctuation_chars=";&|()<>")
    lexer.whitespace_split = True
    try:
        arguments = list(lexer)
    except ValueError:
        return [], {}
    if any(set(argument) <= set(lexer.punctuation_chars) for argument in arguments):
        return [], {}
    environment = {}
    for index, argument in enumerate(arguments):
        assignment = SHELL_ASSIGNMENT.fullmatch(argument)
        if assignment is None:
            return arguments[index:], environment
        environment[assignment[1]] = assignment[2]
    return [], environment


def _manifest_script_watches(package_root: Path, script: str) -> bool:
    manifest = package_root / "package.json"
    if manifest.is_symlink():
        return False
    try:
        package = json.loads(manifest.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return False
    scripts = package.get("scripts") if isinstance(package, dict) else None
    command = scripts.get(script) if isinstance(scripts, dict) else None
    if not isinstance(command, str):
        return False
    arguments, _ = parse_dev_command(command)
    return node_command_watches(arguments)

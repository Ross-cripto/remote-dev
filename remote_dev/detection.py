import json
import re
import shlex
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import Any

from .config import (
    CONFIG_FILE,
    GENERATED_COMPOSE_FILE,
    MAX_TCP_PORT,
    NODE_IMAGE,
    NODE_SOURCE,
    WORKSPACE,
    ConfigError,
    Forward,
    Project,
    _base_service,
    _escape_for_compose,
    _validate_name,
    node_command_watches,
    node_labels,
    parse_dev_command,
    starts_vite_dev_server,
)

COMPOSE_FILES = (
    "compose.yaml",
    "compose.yml",
    "docker-compose.yaml",
    "docker-compose.yml",
)
DEVELOPMENT_COMPOSE_FILES = (
    "compose.dev.yaml",
    "compose.dev.yml",
    "compose.development.yaml",
    "compose.development.yml",
    "docker-compose.dev.yaml",
    "docker-compose.dev.yml",
    "docker-compose.development.yaml",
    "docker-compose.development.yml",
    "compose.override.yaml",
    "compose.override.yml",
    "docker-compose.override.yaml",
    "docker-compose.override.yml",
)
INFRASTRUCTURE_IMAGES = {
    "postgres",
    "redis",
    "mysql",
    "mariadb",
    "mongo",
    "memcached",
    "rabbitmq",
    "minio/minio",
    "valkey/valkey",
}
WORKSPACE_MANIFEST = "pnpm-workspace.yaml"
CONVENTIONAL_APP_PATHS = (
    "frontend",
    "backend",
    "web",
    "api",
    "server",
    "app",
    "apps/*",
)
WORKSPACE_PATTERN = re.compile(r"[A-Za-z0-9_.*?/-]+")
PACKAGE_MANAGER_PATTERN = re.compile(
    r"(npm|pnpm|yarn)@(\d+)\.\d+\.\d+(?:[-+][A-Za-z0-9.+-]+)?"
)
NODE_VERSION_PATTERN = re.compile(r"v?(\d+)(?:\.\d+){0,2}")
STATIC_IMAGE_NAME = re.compile(
    r"([a-z0-9./-]+)(?::[A-Za-z0-9_.-]+)?(?:@sha256:[a-f0-9]{64})?"
)
SERVICE_HEADING = re.compile(r"([A-Za-z0-9_.-]+):\s*(?:#.*)?$")
COMPOSE_TOP_LEVEL_KEYS = {
    "name",
    "version",
    "services",
    "volumes",
    "networks",
    "secrets",
    "configs",
}
CUSTOM_INFRASTRUCTURE_KEYS = {
    "<<",
    "build",
    "extends",
    "entrypoint",
    "networks",
    "network_mode",
    "profiles",
}
LOCKFILES = {
    "npm": ("package-lock.json", "npm-shrinkwrap.json"),
    "pnpm": ("pnpm-lock.yaml",),
    "yarn": ("yarn.lock",),
    "bun": ("bun.lock", "bun.lockb"),
}
GENERIC_NODE_COMMANDS = {"node", "tsx", "nodemon"}
GENERIC_NODE_PORT = 3000
VITE_PORT = 5173


@dataclass(frozen=True)
class NodePackage:
    directory: Path
    manifest: dict[str, Any]

    def script(self, name: str) -> str | None:
        scripts = self.manifest.get("scripts")
        script = scripts.get(name) if isinstance(scripts, dict) else None
        if isinstance(script, str) and script.strip():
            return script
        return None


@dataclass(frozen=True)
class NodeApp:
    package: NodePackage
    name: str
    script: str
    port: int
    arguments: tuple[str, ...]
    watches: bool
    ready_for_orchestrator: bool


@dataclass(frozen=True)
class PackageManager:
    name: str
    setup: str | None
    install: str


def detect_project(root: Path, host: str) -> Project:
    host = _validate_name(host, "SSH host alias")
    root = root.resolve()
    compose, override = _compose_paths(root)
    infrastructure = None
    if compose is not None:
        if override is None:
            infrastructure = _infrastructure_services(root, compose)
        if infrastructure is None:
            return _compose_project(host, compose, override)

    root_package = _read_package(root, Path("."))
    workspaces = _workspace_patterns(root, root_package)
    apps = _node_apps(root, root_package, workspaces)
    if not apps:
        if compose is not None:
            return _compose_project(host, compose, override)
        raise ConfigError(
            f"No supported Compose file or recognizable Node dev script was found. "
            f"Add {CONFIG_FILE} with mode = 'commands' for a custom launch command."
        )

    use_root_script = _use_root_script(root_package, workspaces, apps)
    generated, forwards = _node_compose(
        root, root_package, workspaces, apps, use_root_script
    )
    if infrastructure:
        collisions = infrastructure.intersection(generated["services"])
        if collisions:
            raise ConfigError(
                f"Detected Node services overlap with Compose services ({', '.join(sorted(collisions))}). "
                f"Select development services in {CONFIG_FILE}."
            )
    source = ", ".join(
        (app.package.directory / "package.json").as_posix() for app in apps
    )
    if use_root_script:
        source = f"package.json (workspace dev script for {source})"
    if compose is not None:
        source = f"{compose} + {source}"
    return Project(
        host=host,
        mode="node",
        compose_file=compose or GENERATED_COMPOSE_FILE,
        generated_compose=generated,
        forwards=forwards,
        auto_ports=compose is not None,
        detected_from=source,
    )


def _compose_paths(root: Path) -> tuple[str | None, str | None]:
    base = next(
        (name for name in COMPOSE_FILES if _project_file(root, Path(name))), None
    )
    development = [
        name for name in DEVELOPMENT_COMPOSE_FILES if _project_file(root, Path(name))
    ]
    if len(development) > 1:
        raise ConfigError(
            f"Several development Compose files were found ({', '.join(development)}). "
            f"Select one with compose_file in {CONFIG_FILE}."
        )
    if not development:
        return base, None
    if base is None:
        return development[0], None
    return base, development[0]


def _compose_project(host: str, compose: str, override: str | None) -> Project:
    source = f"{compose} + {override}" if override else compose
    return Project(host, "compose", compose, None, (), True, source, override)


def _project_file(root: Path, relative: Path) -> bool:
    path = root / relative
    if not path.is_file():
        return False
    if path.resolve() != path:
        raise ConfigError(
            f"Autodetection requires a regular file in the project: {relative}"
        )
    return True


def _read_project_file(root: Path, relative: Path) -> str:
    try:
        return (root / relative).read_text(encoding="utf-8")
    except (OSError, UnicodeError) as error:
        raise ConfigError(f"Cannot read {relative}: {error}") from error


def _infrastructure_services(root: Path, compose: str) -> set[str] | None:
    lines = _read_project_file(root, Path(compose)).splitlines()
    lines = [
        line for line in lines if line.strip() and not line.lstrip().startswith("#")
    ]
    services_indent = None
    properties_indent = None
    services: dict[str, str | None] = {}
    in_services = False
    for line in lines:
        indent = len(line) - len(line.lstrip(" "))
        content = line[indent:]
        if indent == 0:
            key, separator, _ = content.partition(":")
            if not separator or not _known_compose_key(key):
                return None
            if _top_level_services(content):
                if in_services or services:
                    return None
                in_services = True
            else:
                in_services = False
            continue
        if not in_services:
            continue
        if services_indent is None:
            services_indent = indent
        if indent == services_indent:
            heading = SERVICE_HEADING.fullmatch(content)
            if heading is None or heading[1] in services:
                return None
            name = heading[1]
            services[name] = None
            properties_indent = None
            continue
        if indent < services_indent:
            return None
        if properties_indent is None:
            properties_indent = indent
        if indent != properties_indent:
            continue
        key, separator, value = content.partition(":")
        if not separator or not re.fullmatch(r"[a-z_]+", key.strip()):
            return None
        if key.strip() in CUSTOM_INFRASTRUCTURE_KEYS:
            return None
        if key == "image":
            if services[name] is not None or not _infrastructure_image(value):
                return None
            services[name] = value
    if not services or any(image is None for image in services.values()):
        return None
    return set(services)


def _top_level_services(content: str) -> bool:
    heading = SERVICE_HEADING.fullmatch(content)
    return heading is not None and heading[1] == "services"


def _known_compose_key(key: str) -> bool:
    return key in COMPOSE_TOP_LEVEL_KEYS or key.startswith("x-")


def _infrastructure_image(value: str) -> bool:
    try:
        words = shlex.split(value, comments=True)
    except ValueError:
        return False
    if len(words) != 1:
        return False
    image = STATIC_IMAGE_NAME.fullmatch(words[0])
    if image is None:
        return False
    name = image[1].removeprefix("docker.io/").removeprefix("library/")
    return name in INFRASTRUCTURE_IMAGES


def _read_package(root: Path, directory: Path) -> NodePackage | None:
    relative = directory / "package.json"
    if not _project_file(root, relative):
        return None
    try:
        manifest = json.loads(_read_project_file(root, relative))
    except ValueError as error:
        raise ConfigError(f"Invalid JSON in {relative}") from error
    if not isinstance(manifest, dict):
        raise ConfigError(f"Expected a JSON object in {relative}")
    return NodePackage(directory, manifest)


def _workspace_patterns(root: Path, package: NodePackage | None) -> tuple[str, ...]:
    if _project_file(root, Path(WORKSPACE_MANIFEST)):
        return tuple(
            _pnpm_workspace_patterns(_read_project_file(root, Path(WORKSPACE_MANIFEST)))
        )
    if package is None:
        return ()
    workspaces = package.manifest.get("workspaces", [])
    if isinstance(workspaces, dict):
        workspaces = workspaces.get("packages", [])
    if not isinstance(workspaces, list) or not all(
        isinstance(path, str) for path in workspaces
    ):
        raise ConfigError(
            f"Expected a list of workspace paths in package.json. Use {CONFIG_FILE} to select an app."
        )
    return tuple(workspaces)


def _pnpm_workspace_patterns(manifest: str) -> list[str]:
    patterns = []
    in_packages = False
    for line in manifest.splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if line.startswith("packages:"):
            in_packages = True
            if line.partition(":")[2].split("#", 1)[0].strip() not in ("", "[]"):
                raise ConfigError(
                    f"Use a packages block list in {WORKSPACE_MANIFEST} or configure {CONFIG_FILE}."
                )
            continue
        if not line.startswith((" ", "- ")):
            in_packages = False
        if not in_packages:
            continue
        try:
            words = shlex.split(line, comments=True)
        except ValueError:
            words = []
        if len(words) != 2 or words[0] != "-":
            raise ConfigError(
                f"Unrecognized packages list in {WORKSPACE_MANIFEST}. Use {CONFIG_FILE} to select an app."
            )
        patterns.append(words[1])
    return patterns


def _workspace_directories(root: Path, patterns: tuple[str, ...]) -> list[Path]:
    directories = set()
    excluded_directories = set()
    for pattern in patterns:
        is_exclusion = pattern.startswith("!")
        glob_pattern = pattern.removeprefix("!")
        path = Path(glob_pattern)
        if not _supported_workspace_pattern(glob_pattern, path):
            raise ConfigError(
                f"Workspace path pattern {pattern!r} needs an explicit app selection in {CONFIG_FILE}."
            )
        matches = excluded_directories if is_exclusion else directories
        for directory in root.glob(glob_pattern):
            if (
                directory != root
                and directory.is_dir()
                and directory.resolve() == directory
            ):
                matches.add(directory.relative_to(root))
    return sorted(directories - excluded_directories)


def _supported_workspace_pattern(pattern: str, path: Path) -> bool:
    if path.is_absolute() or ".." in path.parts or "**" in pattern:
        return False
    if any(part.startswith(".") or part == "node_modules" for part in path.parts):
        return False
    return WORKSPACE_PATTERN.fullmatch(pattern) is not None


def _node_apps(
    root: Path, root_package: NodePackage | None, workspaces: tuple[str, ...]
) -> list[NodeApp]:
    if root_package is not None:
        app = _node_app(root_package, "web")
        if app is not None:
            return [app]
    apps = []
    names = set()
    for directory in _workspace_directories(root, workspaces or CONVENTIONAL_APP_PATHS):
        package = _read_package(root, directory)
        if package is None:
            continue
        name = re.sub(r"[^a-z0-9_-]+", "-", directory.as_posix().lower()).strip("-_")
        app = _node_app(package, _validate_name(name, "Service name"))
        if app is None:
            continue
        if name in names:
            raise ConfigError(
                f"Several detected apps use the service name {name!r}. Select apps in {CONFIG_FILE}."
            )
        names.add(name)
        apps.append(app)
    return apps


def _node_app(package: NodePackage, name: str) -> NodeApp | None:
    for script in ("dev", "start:dev"):
        app = _node_script_app(package, name, script)
        if app is not None:
            return app
    return None


def _node_script_app(package: NodePackage, name: str, script: str) -> NodeApp | None:
    arguments, variables = parse_dev_command(package.script(script) or "")
    if not arguments:
        return None
    tool, *options = arguments
    if not _supported_dev_command(tool, options):
        return None

    port = _dev_port(tool, options, variables)
    extra_arguments: list[str] = []
    ready = True
    if tool == "vite":
        host = _option_value(options, ("--host",))
        ready = host in ("", "0.0.0.0", "::")
        if not ready:
            extra_arguments.extend(("--host", "0.0.0.0"))
        if _option_value(options, ("--port",)) is None:
            extra_arguments.extend(("--port", str(port)))
        if "--strictPort" not in options:
            extra_arguments.append("--strictPort")
    elif tool == "next":
        host = _option_value(options, ("--hostname", "-H"))
        ready = host in (None, "0.0.0.0", "::")
        if not ready:
            extra_arguments.extend(("--hostname", "0.0.0.0"))
    return NodeApp(
        package,
        name,
        script,
        port,
        tuple(extra_arguments),
        node_command_watches(arguments),
        ready,
    )


def _supported_dev_command(tool: str, options: list[str]) -> bool:
    if tool == "vite":
        return starts_vite_dev_server(options)
    if tool == "next":
        return options[:1] == ["dev"]
    if tool == "nest":
        return options[:1] == ["start"]
    return tool in GENERIC_NODE_COMMANDS and bool(options)


def _option_value(options: list[str], names: tuple[str, ...]) -> str | None:
    for index in range(len(options) - 1, -1, -1):
        option = options[index]
        flag, equals, value = option.partition("=")
        if flag not in names:
            continue
        if equals:
            return value
        if index + 1 < len(options) and not options[index + 1].startswith("-"):
            return options[index + 1]
        return ""
    return None


def _dev_port(tool: str, options: list[str], environment: dict[str, str]) -> int:
    declared_port = None
    if tool == "next":
        declared_port = _option_value(options, ("--port", "-p"))
    elif tool != "nest":
        declared_port = _option_value(options, ("--port",))
    if declared_port is None and tool != "vite":
        declared_port = environment.get("PORT")
    if declared_port is None:
        return VITE_PORT if tool == "vite" else GENERIC_NODE_PORT
    if (
        not re.fullmatch(r"[0-9]{1,5}", declared_port)
        or not 1 <= int(declared_port) <= MAX_TCP_PORT
    ):
        raise ConfigError(
            f"Cannot determine the port from the Node dev script. Set it in {CONFIG_FILE}."
        )
    return int(declared_port)


def _use_root_script(
    package: NodePackage | None, workspaces: tuple[str, ...], apps: list[NodeApp]
) -> bool:
    if package is None or not workspaces or apps[0].package.directory == Path("."):
        return False
    if not all(app.script == "dev" for app in apps):
        return False
    arguments, _ = parse_dev_command(package.script("dev") or "")
    if not _workspace_runner(arguments, len(apps)):
        return False
    distinct_ports = {app.port for app in apps}
    return len(distinct_ports) == len(apps) and all(
        app.ready_for_orchestrator for app in apps
    )


def _workspace_runner(arguments: list[str], app_count: int) -> bool:
    if arguments in (["turbo", "dev"], ["turbo", "run", "dev"]):
        return True
    if arguments[:3] == ["npm", "run", "dev"]:
        options = set(arguments[3:])
        return (
            app_count == 1
            and "--workspaces" in options
            and options <= {"--workspaces", "--if-present"}
        )
    if arguments[:1] == ["pnpm"] and arguments[-2:] == ["run", "dev"]:
        options = set(arguments[1:-2])
        is_recursive = bool(options & {"-r", "--recursive"})
        runs_in_parallel = app_count == 1 or "--parallel" in options
        return (
            is_recursive
            and runs_in_parallel
            and options <= {"-r", "--recursive", "--parallel"}
        )
    return False


def _node_compose(
    root: Path,
    root_package: NodePackage | None,
    workspaces: tuple[str, ...],
    apps: list[NodeApp],
    use_root_script: bool,
) -> tuple[dict[str, Any], tuple[Forward, ...]]:
    services: dict[str, Any] = {}
    if use_root_script and root_package is not None:
        services["web"] = _node_service(
            root, root_package, root_package, apps, "dev", ()
        )
        return {"services": services}, tuple(
            Forward(app.name, "web", app.port, None) for app in apps
        )
    for app in apps:
        install_package = (
            root_package if workspaces and root_package is not None else app.package
        )
        services[app.name] = _node_service(
            root,
            app.package,
            install_package,
            [app],
            app.script,
            app.arguments,
        )
    return {"services": services}, tuple(
        Forward(app.name, app.name, app.port, None) for app in apps
    )


def _node_service(
    root: Path,
    package: NodePackage,
    install_package: NodePackage,
    apps: list[NodeApp],
    script: str,
    arguments: tuple[str, ...],
) -> dict[str, Any]:
    ports = [app.port for app in apps]
    environment = {"HOST": "0.0.0.0"}
    if len(ports) == 1:
        environment["PORT"] = str(ports[0])
    run_directory = WORKSPACE / package.directory
    install_directory = WORKSPACE / install_package.directory
    manager = _package_manager(root, install_package)
    service = _base_service(
        PurePosixPath(package.directory.as_posix()), NODE_SOURCE, environment, ports
    )
    service["labels"] = node_labels(all(app.watches for app in apps))
    service["image"] = _node_image(root, install_package)
    startup_steps = [
        f"cp -a {NODE_SOURCE}/. {WORKSPACE}",
        shlex.join(("cd", str(install_directory))),
    ]
    if manager.setup is not None:
        startup_steps.append(manager.setup)
    startup_steps.append(manager.install)
    if run_directory != install_directory:
        startup_steps.append(shlex.join(("cd", str(run_directory))))
    run = ["exec", manager.name, "run", script]
    if arguments and manager.name == "npm":
        run.append("--")
    startup_steps.append(shlex.join((*run, *arguments)))
    service["command"] = [
        "/bin/sh",
        "-c",
        _escape_for_compose(" && ".join(startup_steps)),
    ]
    service["volumes"].append(
        {
            "type": "volume",
            "target": _escape_for_compose(str(install_directory / "node_modules")),
        }
    )
    return service


def _package_manager(root: Path, package: NodePackage) -> PackageManager:
    lockfiles = {
        name
        for name, files in LOCKFILES.items()
        if any(_project_file(root, package.directory / filename) for filename in files)
    }
    declaration = package.manifest.get("packageManager")
    pinned = None
    if declaration is not None:
        if isinstance(declaration, str):
            pinned = PACKAGE_MANAGER_PATTERN.fullmatch(declaration)
        if pinned is None:
            raise ConfigError(
                f"Unsupported packageManager in {package.directory / 'package.json'}. Use {CONFIG_FILE} for custom commands."
            )
        name = pinned[1]
    else:
        if len(lockfiles) > 1:
            raise ConfigError(
                f"Several package manager lockfiles found in {package.directory}. Declare packageManager in package.json."
            )
        name = next(
            iter(lockfiles),
            "pnpm"
            if _project_file(root, package.directory / WORKSPACE_MANIFEST)
            else "npm",
        )

    has_lockfile = name in lockfiles
    if name == "npm":
        setup = "corepack enable npm" if pinned else None
        return PackageManager(
            name, setup, "npm ci" if has_lockfile else "npm install --no-package-lock"
        )
    if name == "pnpm":
        flags = "--frozen-lockfile" if has_lockfile else "--no-frozen-lockfile"
        return PackageManager(name, "corepack enable pnpm", f"pnpm install {flags}")
    if name == "yarn":
        flag = ""
        if has_lockfile:
            modern_yarn = pinned is not None and int(pinned[2]) >= 2
            flag = " --immutable" if modern_yarn else " --frozen-lockfile"
        return PackageManager(name, "corepack enable yarn", f"yarn install{flag}")
    raise ConfigError(
        f"Unsupported package manager {name!r}. Use {CONFIG_FILE} for custom commands."
    )


def _node_image(root: Path, package: NodePackage) -> str:
    for directory in dict.fromkeys((package.directory, Path("."))):
        for filename in (".nvmrc", ".node-version"):
            relative = directory / filename
            if not _project_file(root, relative):
                continue
            value = _read_project_file(root, relative).strip()
            version = NODE_VERSION_PATTERN.fullmatch(value)
            if version is not None and version[1] in {"20", "22", "24"}:
                return f"node:{version[1]}-bookworm-slim"
    return NODE_IMAGE

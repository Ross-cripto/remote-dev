from __future__ import annotations

from dataclasses import asdict, dataclass
import fcntl
from ipaddress import IPv6Address
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tempfile


CONNECTIONS_FILE = "connections.json"
SSH_NAME = re.compile(r"[A-Za-z0-9_][A-Za-z0-9_.-]*")
INVALID_TARGET = "Use a literal OpenSSH alias or user@hostname as the SSH destination."
MINIMUM_PYTHON = (3, 11)
MINIMUM_COMPOSE_MAJOR = 2
APT_DISTRIBUTIONS = {"debian": "docker-compose", "ubuntu": "docker-compose-v2"}
DOCKER_REPOSITORY_PACKAGES = (
    "docker-ce",
    "docker-ce-cli",
    "containerd.io",
    "docker-buildx-plugin",
    "docker-compose-plugin",
)
APT_PACKAGES = (
    "python3",
    "docker.io",
    *APT_DISTRIBUTIONS.values(),
    *DOCKER_REPOSITORY_PACKAGES,
)
PROBE_SCRIPT = """\
export LC_ALL=C
kernel=$(uname -s)
printf 'remote-dev-kernel\t%s\n' "$kernel"
[ "$kernel" = Linux ] || exit 0
if [ -r /etc/os-release ]; then
    . /etc/os-release
elif [ -r /usr/lib/os-release ]; then
    . /usr/lib/os-release
fi
printf 'remote-dev-distro\t%s\t%s\n' "${ID:-}" "${VERSION_ID:-}"
report() {
    label=$1
    shift
    detail=$("$@" 2>&1)
    status=$?
    printf 'remote-dev-%s\t%s\t' "$label" "$status"
    printf '%s' "$detail" | tr '\\n\\r\\t' '   '
    printf '\\n'
}
report python python3 --version
report docker docker info --format '{{.ServerVersion}}'
report compose docker compose version --short
"""
APT_PROBE_SCRIPT = """\
export LC_ALL=C
command -v apt-get >/dev/null && command -v apt-cache >/dev/null || exit 127
privilege=
if command -v sudo >/dev/null; then privilege=sudo; fi
printf 'remote-dev-apt\t%s\t%s\n' "$(id -u)" "$privilege"
apt-cache policy "$@"
"""


@dataclass(frozen=True)
class Connection:
    target: str
    identity: str | None = None

    def __post_init__(self) -> None:
        object.__setattr__(self, "target", _ssh_target(self.target))
        if self.identity is not None:
            object.__setattr__(self, "identity", _identity_path(self.identity))


def _ssh_target(value: str) -> str:
    if not isinstance(value, str):
        raise ValueError(INVALID_TARGET)
    username, separator, hostname = value.rpartition("@")
    if separator and not SSH_NAME.fullmatch(username):
        raise ValueError(INVALID_TARGET)
    if SSH_NAME.fullmatch(hostname):
        return value
    if hostname.startswith("[") and hostname.endswith("]"):
        hostname = hostname[1:-1]
    try:
        address = IPv6Address(hostname)
    except ValueError as error:
        raise ValueError(INVALID_TARGET) from error
    if address.scope_id is not None:
        raise ValueError(INVALID_TARGET)
    return f"{username}{separator}{address}"


def _identity_path(value: str) -> str:
    if not isinstance(value, str) or not value or not value.isprintable():
        raise ValueError("Give --identity the path to a local SSH identity file.")
    if "%" in value or "${" in value:
        raise ValueError(
            "Use a local identity filename without OpenSSH expansion tokens."
        )
    return str(Path(value).expanduser().absolute())


def ssh_options(connection: Connection, *, batch: bool = True) -> list[str]:
    options = [
        "-o",
        f"BatchMode={'yes' if batch else 'no'}",
        "-o",
        f"StrictHostKeyChecking={'yes' if batch else 'ask'}",
        "-o",
        "ConnectTimeout=8",
        "-o",
        "ServerAliveInterval=10",
        "-o",
        "ServerAliveCountMax=3",
    ]
    if connection.identity:
        options.extend(["-i", connection.identity, "-o", "IdentitiesOnly=yes"])
    return options


def _read_connections(state: Path) -> tuple[str | None, dict[str, Connection]]:
    path = state / CONNECTIONS_FILE
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return None, {}
    except ValueError as error:
        raise ValueError(f"Cannot read saved connections in {path}: {error}") from error

    try:
        connections = {
            name: Connection(record["target"], record.get("identity"))
            for name, record in raw["connections"].items()
        }
        default = raw["default"]
        if not isinstance(default, str) or default not in connections:
            raise ValueError("The saved default does not name a registered connection")
    except (AttributeError, KeyError, TypeError, ValueError) as error:
        raise ValueError(f"Invalid saved connections in {path}: {error}") from error
    return default, connections


def load_connection(
    state: Path, label: str | None = None
) -> tuple[str, Connection] | None:
    default, connections = _read_connections(state)
    selected = default if label is None else label
    if selected is not None and selected in connections:
        return selected, connections[selected]
    return None


def _private_lock(path: str, flags: int) -> int:
    return os.open(path, flags | os.O_NOFOLLOW, 0o600)


def _check_label(label: str, saved: Connection | None, connection: Connection) -> None:
    if saved is not None and saved.target != connection.target:
        raise ValueError(
            f"The connection {label} already points to {saved.target}. Choose another --name."
        )


def _save_connection(state: Path, label: str, connection: Connection) -> None:
    state.mkdir(mode=0o700, parents=True, exist_ok=True)
    with open(state / "connections.lock", "a", opener=_private_lock) as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        _, connections = _read_connections(state)
        _check_label(label, connections.get(label), connection)
        connections[label] = connection
        record = {
            "default": label,
            "connections": {name: asdict(value) for name, value in connections.items()},
        }
        temporary = None
        try:
            with tempfile.NamedTemporaryFile(
                mode="w", encoding="utf-8", dir=state, delete=False
            ) as stream:
                temporary = Path(stream.name)
                json.dump(record, stream, indent=2)
                stream.write("\n")
            temporary.replace(state / CONNECTIONS_FILE)
        finally:
            if temporary is not None:
                temporary.unlink(missing_ok=True)


class UnknownHostKey(RuntimeError):
    pass


def _ssh_error(connection: Connection, detail: str) -> RuntimeError:
    message = detail.lower()
    error_type = RuntimeError
    if "identification has changed" in message or "revoked host key" in message:
        guidance = "Verify the server’s current fingerprint with its administrator before updating known_hosts."
    elif "host key" in message:
        command = ["ssh", "-o", "StrictHostKeyChecking=ask"]
        if connection.identity:
            command.extend(["-i", connection.identity])
        command.extend(["--", connection.target])
        guidance = (
            "Verify the server fingerprint with its administrator, then establish trust "
            f"with {shlex.join(command)} and retry."
        )
        if re.search(r"\bno \S+ host key is known\b", message):
            error_type = UnknownHostKey
    elif "permission denied" in message or "authentication" in message:
        guidance = (
            "Check the SSH username and the public key authorized on the server, "
            "then supply --identity or load the key with ssh-agent and ssh-add."
        )
    else:
        guidance = (
            "Check the destination, configured SSH port and network reachability."
        )
    return error_type(
        f"SSH connection to {connection.target} failed. {guidance}\n{detail}"
    )


def _ssh(
    connection: Connection, command: str, *, terminal: bool = False, batch: bool = True
) -> subprocess.CompletedProcess[str]:
    arguments = [
        "ssh",
        *ssh_options(connection, batch=batch),
        "-tt" if terminal else "-T",
        "-o",
        "ClearAllForwardings=yes",
        "-o",
        "ControlPath=none",
        "--",
        connection.target,
        command,
    ]
    if terminal:
        result = subprocess.run(arguments, text=True)
    else:
        result = subprocess.run(
            arguments,
            stdin=subprocess.DEVNULL,
            capture_output=True,
            text=True,
            errors="replace",
        )
    if result.returncode == 255:
        raise _ssh_error(connection, (result.stderr or "").strip())
    return result


@dataclass(frozen=True)
class ProbeResult:
    status: int
    detail: str

    @property
    def available(self) -> bool:
        return self.status == 0 and bool(self.detail)


@dataclass(frozen=True)
class Host:
    distro: str
    release: str
    python: ProbeResult
    docker: ProbeResult
    compose: ProbeResult

    @property
    def name(self) -> str:
        return (
            f"{self.distro.capitalize()} {self.release}".strip()
            if self.distro
            else "Linux"
        )

    def issues(self) -> list[str]:
        issues = []
        if not self.python.available or not _supported_python(self.python.detail):
            issues.append(
                "Python 3.11 or newer is required as the server’s python3 command."
            )
        if self.docker.status == 127:
            issues.append("Docker Engine is missing.")
        elif not self.docker.available:
            issues.append(_docker_guidance(self.docker.detail))
        if not self.compose.available or not _supported_compose(self.compose.detail):
            issues.append(
                "The Docker Compose v2 or newer CLI plugin is required (docker compose)."
            )
        return issues


def _supported_python(version: str) -> bool:
    match = re.match(r"(?:Python )?(\d+)\.(\d+)\.(\d+)(.*)", version)
    if match is None:
        return False
    major_minor = (int(match[1]), int(match[2]))
    return major_minor >= MINIMUM_PYTHON and not match[4].startswith(
        ("a", "b", "rc", "~")
    )


def _supported_compose(version: str) -> bool:
    match = re.match(r"(?:\d+:)?v?(\d+)\.", version)
    return match is not None and int(match[1]) >= MINIMUM_COMPOSE_MAJOR


def _docker_guidance(detail: str) -> str:
    if "permission denied" in detail.lower() or "access denied" in detail.lower():
        guidance = (
            "Docker Engine is inaccessible to this SSH account. Ask the administrator "
            "to configure rootless Docker for the account, or explicitly grant access "
            "to the existing Docker daemon. Membership in the docker group grants "
            "root-equivalent privileges. Reconnect after any permission changes."
        )
    else:
        guidance = (
            "This SSH account cannot reach a running Docker Engine. Check its Docker "
            "context and daemon (systemctl status docker, or systemctl --user status docker "
            "for rootless Docker), then verify that docker info works over SSH without sudo."
        )
    return f"{guidance}\n{detail}" if detail else guidance


def _inspect(connection: Connection) -> Host:
    result = _ssh(connection, shlex.join(["sh", "-c", PROBE_SCRIPT]))
    if result.returncode:
        raise RuntimeError(
            f"Server checks failed on {connection.target}: {result.stderr.strip()}"
        )
    fields = {}
    for line in result.stdout.splitlines():
        if line.startswith("remote-dev-"):
            name, _, value = line.partition("\t")
            fields[name] = value
    try:
        kernel = fields["remote-dev-kernel"]
        if kernel != "Linux":
            raise RuntimeError(
                f"{connection.target} runs {kernel}. Remote Dev requires a Linux server."
            )
        distro, release = fields["remote-dev-distro"].split("\t", 1)
        return Host(
            distro,
            release,
            _parse_probe(fields["remote-dev-python"]),
            _parse_probe(fields["remote-dev-docker"]),
            _parse_probe(fields["remote-dev-compose"]),
        )
    except (KeyError, ValueError) as error:
        raise RuntimeError(
            f"Could not read server checks from {connection.target}."
        ) from error


def _inspect_with_first_use_prompt(connection: Connection) -> Host:
    try:
        return _inspect(connection)
    except UnknownHostKey:
        if not _has_terminal():
            raise
    print(
        "Verify the server fingerprint with its administrator before accepting the OpenSSH prompt.",
        flush=True,
    )
    try:
        result = _ssh(connection, "true", terminal=True, batch=False)
    except RuntimeError as error:
        raise RuntimeError(
            "SSH host verification failed. Review the OpenSSH output above."
        ) from error
    if result.returncode:
        raise RuntimeError("The SSH server could not run the connection check.")
    return _inspect(connection)


def _parse_probe(value: str) -> ProbeResult:
    status, detail = value.split("\t", 1)
    return ProbeResult(int(status), detail.strip())


@dataclass(frozen=True)
class AptPackage:
    installed: str | None = None
    candidate: str | None = None


def _apt_packages(output: str) -> dict[str, AptPackage]:
    packages = {}
    for match in re.finditer(
        r"^([a-z0-9.-]+):\n +Installed: (.+)\n +Candidate: (.+)$",
        output,
        flags=re.MULTILINE,
    ):
        name, installed, candidate = match.groups()
        packages[name] = AptPackage(
            None if installed == "(none)" else installed,
            None if candidate == "(none)" else candidate,
        )
    return packages


def _missing_candidate(package: str) -> RuntimeError:
    return RuntimeError(
        f"No compatible APT candidate is available for {package}. "
        "Ask an administrator to check the host’s configured package repositories, "
        "run sudo apt-get update, or install the requirement manually and retry."
    )


def _required_packages(host: Host, packages: dict[str, AptPackage]) -> list[str]:
    required = []
    if not host.python.available or not _supported_python(host.python.detail):
        python = packages.get("python3", AptPackage()).candidate
        if not python or not _supported_python(python):
            raise _missing_candidate("Python 3.11+ as python3")
        required.append("python3")

    if host.compose.available and _supported_compose(host.compose.detail):
        return required

    distro_compose = APT_DISTRIBUTIONS[host.distro]
    distro_docker = packages.get("docker.io", AptPackage())
    distro_candidate = packages.get(distro_compose, AptPackage()).candidate
    vendor_cli = packages.get("docker-ce-cli", AptPackage())
    vendor_compose = packages.get("docker-compose-plugin", AptPackage()).candidate
    vendor_available = bool(vendor_compose and _supported_compose(vendor_compose))
    distro_available = bool(distro_candidate and _supported_compose(distro_candidate))

    if host.docker.available:
        if vendor_cli.installed and vendor_available:
            return [*required, "docker-compose-plugin"]
        if distro_docker.installed and distro_available:
            return [*required, distro_compose]
        raise _missing_candidate(
            "a Compose plugin compatible with the existing Docker Engine"
        )

    if distro_docker.candidate and distro_available:
        return [*required, "docker.io", distro_compose]
    if vendor_available and all(
        packages.get(package, AptPackage()).candidate
        for package in DOCKER_REPOSITORY_PACKAGES
    ):
        return [*required, *DOCKER_REPOSITORY_PACKAGES]
    raise _missing_candidate("Docker Engine and Docker Compose v2 or newer")


def _prepare(connection: Connection, host: Host) -> Host:
    if host.distro not in APT_DISTRIBUTIONS:
        raise RuntimeError(
            "Automatic preparation uses existing APT repositories on Debian and Ubuntu. "
            f"Install the missing requirements on {host.name} and retry."
        )
    if not host.docker.available and host.docker.status != 127:
        raise RuntimeError(
            "Resolve Docker Engine access before preparing remaining packages."
        )
    availability = _ssh(
        connection, shlex.join(["sh", "-c", APT_PROBE_SCRIPT, "sh", *APT_PACKAGES])
    )
    if availability.returncode:
        raise RuntimeError(
            "APT and its package indexes are required for automatic preparation."
        )
    prefix = _apt_execution_prefix(availability.stdout)
    packages = _required_packages(host, _apt_packages(availability.stdout))
    simulation = _ssh(
        connection,
        shlex.join(["apt-get", "--simulate", "--no-remove", "install", *packages]),
    )
    if simulation.returncode:
        detail = (simulation.stderr or simulation.stdout).strip()
        raise RuntimeError(f"APT could not plan the package installation.\n{detail}")

    command = shlex.join(
        [*prefix, "apt-get", "--no-remove", "--yes", "install", *packages]
    )
    proposal = (
        f"Proposed changes on {connection.target} ({host.name})\n"
        f"{command}\n\n{simulation.stdout.strip()}"
    )
    _confirm_install(proposal)
    installation = _ssh(connection, command, terminal=True)
    if installation.returncode:
        raise RuntimeError(
            "APT installation failed. Check the output above. The SSH account needs root "
            "or permission to run the displayed command with sudo. Run the setup again "
            "after resolving the reported error."
        )
    return _inspect(connection)


def _apt_execution_prefix(output: str) -> list[str]:
    for line in output.splitlines():
        if not line.startswith("remote-dev-apt\t"):
            continue
        _, _, credentials = line.partition("\t")
        user_id, _, privilege = credentials.partition("\t")
        if user_id == "0":
            return []
        if user_id.isdecimal() and privilege == "sudo":
            return ["sudo", "--"]
        raise RuntimeError(
            "Ask a server administrator with root or sudo access to install the missing packages."
        )
    raise RuntimeError("Could not determine how to run APT on the server.")


def _has_terminal() -> bool:
    return sys.stdin.isatty() and sys.stdout.isatty()


def _confirm_install(proposal: str) -> None:
    if not _has_terminal():
        raise RuntimeError(
            f"{proposal}\n\nRun with --prepare in an interactive terminal to authorize these changes."
        )
    print(proposal)
    try:
        choice = input("Type yes to authorize these package changes. ")
    except (EOFError, KeyboardInterrupt):
        choice = ""
    if choice.strip().lower() != "yes":
        raise RuntimeError("Server preparation canceled.")


def onboard(
    state: Path,
    target: str | None,
    *,
    name: str | None,
    identity: str | None,
    prepare: bool,
) -> str:
    if target is None:
        existing = load_connection(state, name)
        if existing is None:
            if name is not None:
                raise ValueError(f"No saved connection named {name}.")
            raise ValueError(
                "Give an SSH alias or user@hostname to register a server first."
            )
        label, saved = existing
        connection = Connection(
            saved.target, saved.identity if identity is None else identity
        )
    else:
        connection = Connection(target, identity)
        if name is not None and not SSH_NAME.fullmatch(name):
            raise ValueError(
                "A connection name may contain letters, numbers, dots, underscores and hyphens."
            )
        label = connection.target if name is None else name
        existing = load_connection(state, label)
        if existing is not None:
            _, saved = existing
            _check_label(label, saved, connection)
            if identity is None:
                connection = saved
    if connection.identity and not Path(connection.identity).is_file():
        raise ValueError(
            f"Local SSH identity file does not exist: {connection.identity}"
        )

    host = _inspect_with_first_use_prompt(connection)
    issues = host.issues()
    if issues and prepare:
        try:
            host = _prepare(connection, host)
        except RuntimeError as error:
            raise RuntimeError(
                f"{host.name} on {connection.target}\n"
                + "\n".join([*issues, str(error)])
            ) from error
        issues = host.issues()
    if issues:
        guidance = (
            "Run again with --prepare to preview and authorize available package changes."
            if not prepare and host.distro in APT_DISTRIBUTIONS
            else "Resolve the requirements above and retry."
        )
        raise RuntimeError(
            f"{host.name} on {connection.target}\n" + "\n".join([*issues, guidance])
        )

    _save_connection(state, label, connection)
    return (
        f"Connected to {connection.target} as {label} ({host.name}).\n"
        f"{host.python.detail}, Docker Engine {host.docker.detail}, Docker Compose {host.compose.detail}."
    )

from contextlib import redirect_stdout
import io
import os
from pathlib import Path
import shlex
import shutil
import stat
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from remote_dev.connection import Connection, load_connection, onboard, ssh_options


READY_HOST = """remote-dev-kernel	Linux
remote-dev-distro	ubuntu	24.04
remote-dev-python	0	Python 3.12.3
remote-dev-docker	0	28.1.0
remote-dev-compose	0	5.1.0
"""
EMPTY_HOST = """remote-dev-kernel	Linux
remote-dev-distro	ubuntu	24.04
remote-dev-python	127\t
remote-dev-docker	127\t
remote-dev-compose	127\t
"""
UBUNTU_CANDIDATES = """remote-dev-apt	1000	sudo
python3:
  Installed: (none)
  Candidate: 3.12.3-0ubuntu2
docker.io:
  Installed: (none)
  Candidate: 28.2.2-0ubuntu1
docker-compose-v2:
  Installed: (none)
  Candidate: 2.37.1-0ubuntu1
"""
APT_SIMULATION = """The following NEW packages will be installed:
  docker-compose-v2 docker.io python3
0 upgraded, 3 newly installed, 0 to remove and 0 not upgraded.
Inst docker.io (28.2.2-0ubuntu1 Ubuntu:24.04/noble [amd64])
"""


def completed(output="", *, error="", status=0):
    return subprocess.CompletedProcess([], status, output, error)


class ConnectionTests(unittest.TestCase):
    def setUp(self):
        temporary_directory = tempfile.TemporaryDirectory()
        self.addCleanup(temporary_directory.cleanup)
        self.root = Path(temporary_directory.name)
        self.state = self.root / "standalone-v1"

    def test_ssh_accepts_literal_targets_and_keeps_identity_a_local_argument(self):
        key_path = self.root / "test key"
        connection = Connection("dev@example.test", str(key_path))

        self.assertEqual(connection.target, "dev@example.test")
        self.assertEqual(Connection("lab").target, "lab")
        self.assertEqual(Connection("dev@[::1]").target, "dev@::1")
        options = ssh_options(connection)
        self.assertEqual(options[options.index("-i") + 1], str(key_path))
        self.assertIn("BatchMode=yes", options)
        self.assertIn("StrictHostKeyChecking=yes", options)
        self.assertNotIn("/dev/null", options)

    def test_ssh_rejects_shell_fragments_and_options(self):
        for invalid in (
            "-F",
            "host -J jump",
            "a@b@c",
            "ssh://host",
            "host; date",
            "dev@[::1",
        ):
            with self.subTest(target=invalid), self.assertRaises(ValueError):
                Connection(invalid)
        with self.assertRaises(ValueError):
            Connection("lab", "a\nprivate key")

    @patch("remote_dev.connection.subprocess.run")
    def test_ready_host_is_saved_privately_and_reused_without_remote_changes(self, run):
        key_path = self.root / "test key"
        key_path.write_text("secret key material", encoding="utf-8")
        run.return_value = completed("login banner\n" + READY_HOST)

        summary = onboard(
            self.state,
            "dev@example.test",
            name="lab",
            identity=str(key_path),
            prepare=True,
        )
        repeated = onboard(self.state, None, name=None, identity=None, prepare=False)

        self.assertIn("Ubuntu 24.04", summary)
        self.assertIn("3.12.3", repeated)
        self.assertIn("5.1.0", repeated)
        self.assertEqual(
            load_connection(self.state),
            ("lab", Connection("dev@example.test", str(key_path))),
        )
        self.assertIsNone(load_connection(self.state, "unknown"))
        self.assertEqual(self.state.stat().st_mode & 0o777, 0o700)
        for path in self.state.glob("*.json"):
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            self.assertNotIn("secret key material", path.read_text(encoding="utf-8"))
        self.assertEqual(run.call_count, 2)
        for call in run.call_args_list:
            command = call.args[0]
            self.assertEqual(command[command.index("--") + 1], "dev@example.test")
            self.assertEqual(command[command.index("-i") + 1], str(key_path))
            self.assertNotIn("apt-get", command[-1])

    @patch("remote_dev.connection.subprocess.run")
    def test_other_saved_labels_remain_available_when_default_changes(self, run):
        run.return_value = completed(READY_HOST)

        onboard(self.state, "old", name=None, identity=None, prepare=False)
        onboard(self.state, "new", name="default-lab", identity=None, prepare=False)

        self.assertEqual(
            load_connection(self.state), ("default-lab", Connection("new"))
        )
        self.assertEqual(load_connection(self.state, "old"), ("old", Connection("old")))

    @patch("remote_dev.connection.subprocess.run")
    def test_a_saved_label_cannot_be_rebound_to_a_different_destination(self, run):
        run.return_value = completed(READY_HOST)
        onboard(self.state, "old", name="lab", identity=None, prepare=False)

        with self.assertRaisesRegex(ValueError, "already points to old"):
            onboard(self.state, "new", name="lab", identity=None, prepare=False)

        self.assertEqual(load_connection(self.state, "lab"), ("lab", Connection("old")))
        self.assertEqual(run.call_count, 1)

    @patch("remote_dev.connection.subprocess.run")
    def test_missing_python_does_not_prevent_docker_diagnostics(self, run):
        run.return_value = completed(EMPTY_HOST)

        with self.assertRaisesRegex(
            RuntimeError, r"(?s)Python 3\.11.*Docker Engine.*Compose.*--prepare"
        ):
            onboard(self.state, "lab", name=None, identity=None, prepare=False)

        self.assertIsNone(load_connection(self.state))
        self.assertEqual(run.call_count, 1)
        self.assertTrue(run.call_args.args[0][-1].startswith("sh -c "))

    def test_remote_shell_probe_runs_without_python_and_uses_docker_as_ssh_user(self):
        commands = self.root / "bin"
        commands.mkdir()
        uname = commands / "uname"
        uname.write_text("#!/bin/sh\nprintf 'Linux\\n'\n", encoding="utf-8")
        uname.chmod(0o700)
        docker = commands / "docker"
        docker.write_text(
            """#!/bin/sh
case "$*" in
    "info --format {{.ServerVersion}}") printf '29.0.0\\n' ;;
    "compose version --short") printf 'v5.1.0\\n' ;;
    *) exit 1 ;;
esac
""",
            encoding="utf-8",
        )
        docker.chmod(0o700)
        (commands / "tr").symlink_to(shutil.which("tr"))
        (commands / "sh").symlink_to(shutil.which("sh"))
        run = subprocess.run

        def run_remote_shell(arguments, **options):
            environment = {**os.environ, "PATH": str(commands)}
            return run(shlex.split(arguments[-1]), env=environment, **options)

        with (
            patch("remote_dev.connection.subprocess.run", side_effect=run_remote_shell),
            self.assertRaisesRegex(RuntimeError, "Python 3.11") as error,
        ):
            onboard(self.state, "lab", name=None, identity=None, prepare=False)
        self.assertNotIn("Docker Engine", str(error.exception))
        self.assertNotIn("CLI plugin", str(error.exception))

    @patch("remote_dev.connection.subprocess.run")
    def test_docker_permission_denial_points_to_access_for_ssh_account(self, run):
        inspection = READY_HOST.replace(
            "remote-dev-docker\t0\t28.1.0",
            "remote-dev-docker\t1\tpermission denied while trying to connect to the Docker daemon socket",
        )
        run.return_value = completed(inspection)

        with self.assertRaisesRegex(
            RuntimeError, r"(?s)SSH account.*rootless.*root-equivalent"
        ):
            onboard(self.state, "lab", name=None, identity=None, prepare=True)

        self.assertIsNone(load_connection(self.state))
        self.assertEqual(run.call_count, 1)

    @patch("remote_dev.connection.subprocess.run")
    def test_ssh_key_and_network_failures_include_distinct_next_steps(self, run):
        failures = {
            "Host key verification failed.": "fingerprint",
            "Permission denied (publickey).": "ssh-agent",
            "Connection refused": "reachability",
        }
        for error, guidance in failures.items():
            with (
                self.subTest(error=error),
                self.assertRaisesRegex(RuntimeError, guidance),
            ):
                run.return_value = completed(error=error, status=255)
                onboard(self.state, "lab", name=None, identity=None, prepare=False)
        self.assertIsNone(load_connection(self.state))

    @patch("remote_dev.connection.subprocess.run")
    def test_interactive_unknown_host_uses_native_prompt_then_rechecks_strictly(
        self, run
    ):
        run.side_effect = [
            completed(
                error="No ED25519 host key is known for lab.\nHost key verification failed.",
                status=255,
            ),
            completed(),
            completed(READY_HOST),
        ]

        screen = io.StringIO()
        with (
            redirect_stdout(screen),
            patch.object(screen, "isatty", return_value=True),
            patch("remote_dev.connection.sys.stdin.isatty", return_value=True),
        ):
            onboard(self.state, "lab", name=None, identity=None, prepare=False)

        self.assertIn("Verify the server fingerprint", screen.getvalue())
        initial, verification, recheck = [call.args[0] for call in run.call_args_list]
        self.assertIn("StrictHostKeyChecking=yes", initial)
        self.assertIn("StrictHostKeyChecking=ask", verification)
        self.assertIn("BatchMode=no", verification)
        self.assertIn("StrictHostKeyChecking=yes", recheck)
        self.assertIn("BatchMode=yes", recheck)
        self.assertEqual(load_connection(self.state), ("lab", Connection("lab")))

    @patch("remote_dev.connection.sys.stdin.isatty", return_value=True)
    @patch("remote_dev.connection.sys.stdout.isatty", return_value=True)
    @patch("remote_dev.connection.subprocess.run")
    def test_changed_host_key_is_never_enrolled_interactively(
        self, run, stdout_isatty, stdin_isatty
    ):
        run.return_value = completed(
            error="WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!\nHost key verification failed.",
            status=255,
        )

        with self.assertRaisesRegex(RuntimeError, "current fingerprint"):
            onboard(self.state, "lab", name=None, identity=None, prepare=False)

        self.assertEqual(run.call_count, 1)
        self.assertIsNone(load_connection(self.state))

    @patch("remote_dev.connection.subprocess.run")
    def test_missing_package_sources_include_manual_guidance(self, run):
        run.side_effect = [
            completed(EMPTY_HOST),
            completed("remote-dev-apt\t1000\tsudo\n"),
        ]

        with self.assertRaisesRegex(RuntimeError, r"(?s)APT.*apt-get update"):
            onboard(self.state, "lab", name=None, identity=None, prepare=True)

        self.assertEqual(run.call_count, 2)
        self.assertIsNone(load_connection(self.state))

    @patch("remote_dev.connection.sys.stdin.isatty", return_value=True)
    @patch("builtins.input", return_value="no")
    @patch("remote_dev.connection.subprocess.run")
    def test_declining_package_changes_leaves_host_and_registration_untouched(
        self, run, ask, stdin_isatty
    ):
        run.side_effect = [
            completed(EMPTY_HOST),
            completed(UBUNTU_CANDIDATES),
            completed(APT_SIMULATION),
        ]

        screen = io.StringIO()
        with (
            redirect_stdout(screen),
            patch.object(screen, "isatty", return_value=True),
            self.assertRaisesRegex(RuntimeError, "canceled"),
        ):
            onboard(self.state, "lab", name=None, identity=None, prepare=True)

        self.assertEqual(run.call_count, 3)
        self.assertIn("--simulate", run.call_args.args[0][-1])
        self.assertIsNone(load_connection(self.state))

    @patch("remote_dev.connection.sys.stdin.isatty", return_value=False)
    @patch("builtins.input")
    @patch("remote_dev.connection.subprocess.run")
    def test_noninteractive_preparation_returns_the_plan_before_any_install(
        self, run, ask, stdin_isatty
    ):
        run.side_effect = [
            completed(EMPTY_HOST),
            completed(UBUNTU_CANDIDATES),
            completed(APT_SIMULATION),
        ]

        with self.assertRaisesRegex(RuntimeError, r"(?s)apt-get.*interactive terminal"):
            onboard(self.state, "lab", name=None, identity=None, prepare=True)

        ask.assert_not_called()
        self.assertEqual(run.call_count, 3)
        self.assertIn("--simulate", run.call_args.args[0][-1])
        self.assertIsNone(load_connection(self.state))

    @patch("remote_dev.connection.subprocess.run")
    def test_debian_12_uses_existing_vendor_candidates_for_compose_v2(self, run):
        debian = EMPTY_HOST.replace("ubuntu\t24.04", "debian\t12")
        candidates = """remote-dev-apt	0\t
python3:
  Installed: (none)
  Candidate: 3.11.2-1+b1
docker.io:
  Installed: (none)
  Candidate: 20.10.24+dfsg1-1+deb12u1
docker-compose:
  Installed: (none)
  Candidate: 1.29.2-3
docker-ce:
  Installed: (none)
  Candidate: 5:29.2.1-1~debian.12~bookworm
docker-ce-cli:
  Installed: (none)
  Candidate: 5:29.2.1-1~debian.12~bookworm
containerd.io:
  Installed: (none)
  Candidate: 2.2.1-1~debian.12~bookworm
docker-buildx-plugin:
  Installed: (none)
  Candidate: 0.31.1-1~debian.12~bookworm
docker-compose-plugin:
  Installed: (none)
  Candidate: 5.1.0-1~debian.12~bookworm
"""
        run.side_effect = [
            completed(debian),
            completed(candidates),
            completed(APT_SIMULATION),
        ]

        with (
            patch("remote_dev.connection.sys.stdin.isatty", return_value=False),
            self.assertRaisesRegex(
                RuntimeError,
                r"apt-get --no-remove --yes install python3 docker-ce docker-ce-cli "
                r"containerd\.io docker-buildx-plugin docker-compose-plugin",
            ),
        ):
            onboard(self.state, "lab", name=None, identity=None, prepare=True)

    @patch("remote_dev.connection.subprocess.run")
    def test_missing_sudo_reports_required_administrator_without_confirming(self, run):
        run.side_effect = [
            completed(EMPTY_HOST),
            completed(UBUNTU_CANDIDATES.replace("\t1000\tsudo", "\t1000\t")),
        ]
        with self.assertRaisesRegex(
            RuntimeError, "administrator with root or sudo access"
        ):
            onboard(self.state, "lab", name=None, identity=None, prepare=True)
        self.assertEqual(run.call_count, 2)

    @patch("remote_dev.connection.subprocess.run")
    def test_confirmed_ubuntu_plan_installs_only_required_packages_and_rechecks(
        self, run
    ):
        run.side_effect = [
            completed(EMPTY_HOST),
            completed(UBUNTU_CANDIDATES),
            completed(APT_SIMULATION),
            completed(),
            completed(READY_HOST),
        ]
        screen = io.StringIO()
        with (
            redirect_stdout(screen),
            patch.object(screen, "isatty", return_value=True),
            patch("remote_dev.connection.sys.stdin.isatty", return_value=True),
            patch("builtins.input", return_value="yes"),
        ):
            onboard(self.state, "lab", name=None, identity=None, prepare=True)

        command = "sudo -- apt-get --no-remove --yes install python3 docker.io docker-compose-v2"
        self.assertIn(command, screen.getvalue())
        self.assertIn("Inst docker.io (28.2.2-0ubuntu1", screen.getvalue())
        install = run.call_args_list[3].args[0]
        self.assertIn("-tt", install)
        self.assertEqual(install[-1], command)
        self.assertEqual(run.call_count, 5)
        self.assertEqual(load_connection(self.state), ("lab", Connection("lab")))

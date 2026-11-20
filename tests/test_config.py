import tempfile
import unittest
from pathlib import Path

from remote_dev.config import ConfigError, init_project, load_project


class ProjectConfigTests(unittest.TestCase):
    def setUp(self):
        temporary_directory = tempfile.TemporaryDirectory()
        self.addCleanup(temporary_directory.cleanup)
        self.root = Path(temporary_directory.name)

    def write_config(self, text):
        (self.root / "remote-dev.toml").write_text(text, encoding="utf-8")

    def test_compose_uses_existing_file_and_host_override(self):
        (self.root / "containers").mkdir()
        (self.root / "containers" / "dev.yml").touch()
        self.write_config("""
            host = "lab"
            mode = "compose"
            compose_file = "containers/dev.yml"

            [forwards.frontend]
            service = "web"
            target = 8080
            local = 3000
        """)

        project = load_project(self.root, "lab-backup")

        self.assertEqual(project.host, "lab-backup")
        self.assertEqual(project.compose_file, "containers/dev.yml")
        self.assertIsNone(project.generated_compose)
        (forward,) = project.forwards
        self.assertEqual(
            (forward.name, forward.service, forward.target, forward.local),
            ("frontend", "web", 8080, 3000),
        )

    def test_commands_keep_argv_and_publish_only_on_remote_loopback(self):
        (self.root / "app").mkdir()
        self.write_config("""
            host = "lab"
            mode = "commands"

            [services.web]
            image = "python:3.13-slim"
            directory = "app"
            command = ["python", "-m", "http.server", "8000"]

            [forwards.web]
            service = "web"
            target = 8000
            local = "auto"
        """)

        project = load_project(self.root)
        self.assertEqual(project.compose_file, ".remote-dev.generated.json")
        self.assertIsNone(project.forwards[0].local)
        service = project.generated_compose["services"]["web"]
        self.assertEqual(service["command"], ["python", "-m", "http.server", "8000"])
        self.assertEqual(service["working_dir"], "/workspace/app")
        self.assertEqual(
            service["volumes"],
            [
                {
                    "type": "bind",
                    "source": ".",
                    "target": "/workspace",
                    "read_only": True,
                }
            ],
        )
        self.assertEqual(
            service["ports"],
            [
                {
                    "target": 8000,
                    "protocol": "tcp",
                    "host_ip": "127.0.0.1",
                }
            ],
        )

    def test_command_strings_run_inside_selected_image_and_keep_dollar_literals(self):
        self.write_config("""
            host = "lab"
            mode = "commands"

            [services.web]
            image = "alpine:3"
            command = "echo $GREETING"
            environment = { GREETING = "costs $20" }
        """)

        service = load_project(self.root).generated_compose["services"]["web"]
        self.assertEqual(service["command"], ["/bin/sh", "-c", "echo $$GREETING"])
        self.assertEqual(service["environment"], {"GREETING": "costs $$20"})

    def test_generated_services_cannot_enable_writes_to_source_snapshot(self):
        self.write_config("""
            host = "lab"
            mode = "commands"

            [services.web]
            image = "alpine:3"
            command = "echo Hello"
            read_only = false
        """)

        with self.assertRaisesRegex(ConfigError, "read_only"):
            load_project(self.root)

    def test_node_selects_npm_ci_for_lockfile_and_keeps_dependencies_in_docker(self):
        (self.root / "frontend").mkdir()
        (self.root / "frontend" / "package.json").write_text("{}", encoding="utf-8")
        (self.root / "frontend" / "package-lock.json").touch()
        self.write_config("""
            host = "lab"
            mode = "node"

            [services.frontend]
            directory = "frontend"
            script = "dev:web"

            [forwards.web]
            service = "frontend"
            target = 3000
        """)

        project = load_project(self.root)
        service = project.generated_compose["services"]["frontend"]
        self.assertEqual(service["environment"], {"HOST": "0.0.0.0", "PORT": "3000"})
        self.assertEqual(
            service["command"],
            [
                "/bin/sh",
                "-c",
                'cp -a /remote-source/. /workspace && npm ci && exec npm run -- "$$1"',
                "remote-dev",
                "dev:web",
            ],
        )
        dependency_volume = service["volumes"][1]
        self.assertEqual(dependency_volume["type"], "volume")
        self.assertEqual(
            dependency_volume["target"], "/workspace/frontend/node_modules"
        )
        self.assertIn(dependency_volume["source"], project.generated_compose["volumes"])

    def test_node_uses_install_without_updating_a_missing_lockfile(self):
        (self.root / "package.json").write_text("{}", encoding="utf-8")
        self.write_config("""
            host = "lab"
            mode = "node"
            [services.web]
        """)

        service = load_project(self.root).generated_compose["services"]["web"]
        self.assertIn("npm install --no-package-lock", service["command"][2])

    def test_node_copies_source_to_writable_workspace_even_when_install_is_skipped(
        self,
    ):
        (self.root / "backend").mkdir()
        (self.root / "backend" / "package.json").write_text("{}", encoding="utf-8")
        self.write_config("""
            host = "lab"
            mode = "node"
            [services.web]
            directory = "backend"
            install = "none"
        """)

        service = load_project(self.root).generated_compose["services"]["web"]
        self.assertEqual(service["working_dir"], "/workspace/backend")
        self.assertEqual(
            service["volumes"],
            [
                {
                    "type": "bind",
                    "source": ".",
                    "target": "/remote-source",
                    "read_only": True,
                }
            ],
        )
        self.assertEqual(
            service["command"],
            [
                "/bin/sh",
                "-c",
                'cp -a /remote-source/. /workspace && exec npm run -- "$$1"',
                "remote-dev",
                "dev",
            ],
        )

    def test_config_rejects_unsafe_inputs(self):
        cases = (
            ('host = "-Fbad"\nmode = "compose"', "SSH host alias"),
            ('host = "bad;host"\nmode = "compose"', "SSH host alias"),
            ('host = "lab"\nmode = "unknown"', "mode"),
            (
                'host = "lab"\nmode = "compose"\ncompose_file = "../compose.yaml"',
                "project-relative",
            ),
            (
                'host = "lab"\nmode = "compose"\ncompose_file = ".remote-dev.generated.json"',
                "reserved",
            ),
            (
                """host = "lab"
                mode = "compose"
                [forwards.web]
                service = "-web"
                target = 8000""",
                "service",
            ),
            (
                """host = "lab"
                mode = "compose"
                [forwards.web]
                service = "web"
                target = true""",
                "target",
            ),
            (
                """host = "lab"
                mode = "compose"
                [forwards.web]
                service = "web"
                target = 8000
                local = 65536""",
                "local",
            ),
        )
        for config, error in cases:
            with self.subTest(config=config):
                self.write_config(config)
                with self.assertRaisesRegex(ConfigError, error):
                    load_project(self.root)

    def test_generated_forward_requires_declared_service(self):
        self.write_config("""
            host = "lab"
            mode = "commands"
            [services.web]
            image = "alpine:3"
            command = ["sleep", "10"]
            [forwards.web]
            service = "typo"
            target = 8000
        """)

        with self.assertRaisesRegex(ConfigError, "typo"):
            load_project(self.root)

    def test_forward_local_ports_must_be_unique(self):
        self.write_config("""
            host = "lab"
            mode = "compose"
            [forwards.web]
            service = "web"
            target = 8000
            local = 8000
            [forwards.api]
            service = "api"
            target = 3000
            local = 8000
        """)

        with self.assertRaisesRegex(ConfigError, "8000"):
            load_project(self.root)

    def test_init_creates_mode_template_without_overwriting_config(self):
        for mode in ("compose", "commands", "node"):
            with self.subTest(mode=mode):
                mode_root = self.root / mode
                mode_root.mkdir()
                config_path = init_project(mode_root, "lab-test", mode)
                config = config_path.read_text(encoding="utf-8")
                self.assertIn(f'mode = "{mode}"', config)
                self.assertIn('host = "lab-test"', config)
                with self.assertRaises(FileExistsError):
                    init_project(mode_root, "other", mode)
                self.assertEqual(config_path.read_text(encoding="utf-8"), config)

    def test_examples_load_for_each_mode(self):
        examples = Path(__file__).resolve().parents[1] / "examples"
        for mode in ("compose", "commands", "node"):
            with self.subTest(mode=mode):
                project = load_project(examples / mode)
                self.assertEqual(project.mode, mode)
                self.assertTrue(project.forwards)

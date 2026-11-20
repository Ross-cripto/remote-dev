import json
from pathlib import Path
import tempfile
import unittest

from remote_dev import host


class RuntimeTests(unittest.TestCase):
    def test_automatic_compose_chooses_loopback_ephemeral_ports_for_app_not_database(
        self,
    ):
        config = {
            "services": {
                "web": {"ports": [{"target": 3000, "published": "3000"}]},
                "db": {"ports": [{"target": 5432, "published": "5432"}]},
            },
        }
        forwards = host.configure_automatic_ports(config, [])
        self.assertEqual(forwards, [{"name": "web", "service": "web", "target": 3000}])
        self.assertNotIn("ports", config["services"]["db"])
        self.assertEqual(
            config["services"]["web"]["ports"],
            [{"host_ip": "127.0.0.1", "target": 3000, "protocol": "tcp"}],
        )

    def test_remote_interruption_can_resume_without_overwriting_foreign_edits(self):
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory).resolve()
            previous = self.snapshot(
                workspace, "a", {"app.txt": "before", "unchanged": "keep"}
            )
            desired = self.snapshot(
                workspace, "b", {"app.txt": "after", "unchanged": "keep"}
            )
            runtime = workspace / "runtime-source"
            runtime.mkdir()
            (runtime / "app.txt").write_text("after")
            (runtime / "unchanged").write_text("someone else's change")
            with self.assertRaisesRegex(ValueError, "Remote source changed"):
                host.apply_runtime_source(workspace, previous, desired)
            self.assertEqual(
                (runtime / "unchanged").read_text(), "someone else's change"
            )
            (runtime / "unchanged").write_text("keep")
            host.apply_runtime_source(workspace, previous, desired)
            self.assertEqual(host.inventory(runtime), host.inventory(desired))

    def test_runtime_updates_files_without_replacing_the_bind_mount_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory).resolve()
            first = self.snapshot(
                workspace, "a", {"web/index.html": "before", "stale": "old"}
            )
            second = self.snapshot(
                workspace, "b", {"web/index.html": "after", "added/file": "new"}
            )
            host.apply_runtime_source(workspace, None, first)
            runtime = workspace / "runtime-source"
            inode = (runtime / "web").stat().st_ino
            host.apply_runtime_source(workspace, first, second)
            self.assertEqual((runtime / "web").stat().st_ino, inode)
            self.assertEqual(host.inventory(runtime), host.inventory(second))

    def test_ordinary_commands_reload_only_the_service_with_the_changed_mount(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()

            def bind(name):
                return {"type": "bind", "source": str(root / name)}

            config = {
                "services": {
                    "api": {"command": ["python", "app.py"], "volumes": [bind("api")]},
                    "web": {
                        "command": ["python", "-m", "http.server"],
                        "volumes": [bind("public")],
                    },
                    "db": {"volumes": [{"type": "volume", "source": "storage"}]},
                    "other": {
                        "command": ["python", "app.py"],
                        "volumes": [bind("other")],
                    },
                }
            }
            containers = {name: [name] for name in config["services"]}
            restart = host.services_to_reload(
                root, config, containers, {"api/app.py", "public/index.html"}
            )
            self.assertEqual(restart, ["api"])

    @staticmethod
    def snapshot(workspace, version, contents):
        source = workspace / "snapshots" / (version * 64) / "source"
        source.mkdir(parents=True)
        for filename, content in contents.items():
            path = source / filename
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content)
        (source.parent / "files.json").write_text(json.dumps(host.inventory(source)))
        return source

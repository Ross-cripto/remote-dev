import hashlib
import io
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch

from remote_dev import host, snapshots
from remote_dev.client import Client


class SnapshotTests(unittest.TestCase):
    def test_snapshot_contains_current_source_with_local_index_untouched(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            subprocess.run(["git", "init", "-q", root], check=True)
            source = root / "app.py"
            source.write_text("staged\n")
            subprocess.run(["git", "-C", root, "add", "app.py"], check=True)
            source.write_text("working\n")
            (root / "new.txt").write_text("untracked\n")
            (root / ".env").write_text("PASSWORD=fixture\n")
            (root / "remote-dev.toml").write_text('host = "example"\n')
            index = root / ".git/index"
            before = index.read_bytes()
            with snapshots.capture(root, None) as snapshot:
                first_digest = snapshot.digest
                with tarfile.open(snapshot.path) as archive:
                    self.assertEqual(set(archive.getnames()), {"app.py", "new.txt"})
                    self.assertEqual(archive.extractfile("app.py").read(), b"working\n")
            with snapshots.capture(root, None) as snapshot:
                self.assertEqual(snapshot.digest, first_digest)
            self.assertEqual(index.read_bytes(), before)

    def test_snapshot_refuses_symlink_that_would_copy_outside_the_repo(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            subprocess.run(["git", "init", "-q", root], check=True)
            (root / "outside").symlink_to("/etc/passwd")
            with self.assertRaisesRegex(ValueError, "outside"):
                with snapshots.capture(root, None):
                    self.fail("Captured an unsafe symlink")


class HostTests(unittest.TestCase):
    @staticmethod
    def archive(name, content):
        buffer = io.BytesIO()
        with tarfile.open(fileobj=buffer, mode="w") as archive:
            member = tarfile.TarInfo(name)
            member.size = len(content)
            archive.addfile(member, io.BytesIO(content))
        return buffer.getvalue()

    def test_remote_extract_rejects_path_escape_without_writing_outside_snapshot(self):
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory) / "workspace"
            workspace.mkdir()
            payload = self.archive("../outside", b"do not write")
            with self.assertRaisesRegex(ValueError, "path"):
                host.install_snapshot(
                    workspace, hashlib.sha256(payload).hexdigest(), io.BytesIO(payload)
                )
            self.assertFalse((workspace / "outside").exists())

    def test_remote_snapshot_refuses_reuse_if_a_managed_file_was_changed(self):
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory)
            payload = self.archive("app.txt", b"laptop version")
            digest = hashlib.sha256(payload).hexdigest()
            host.install_snapshot(workspace, digest, io.BytesIO(payload))
            target = host.source_path(workspace, digest) / "app.txt"
            target.write_text("remote change")
            with self.assertRaisesRegex(ValueError, "changed"):
                host.verify_snapshot(workspace, digest)
            self.assertEqual(target.read_text(), "remote change")

    def test_compose_disallows_non_loopback_ports(self):
        config = {"services": {"web": {"ports": [{"target": 80, "published": "8080"}]}}}
        with self.assertRaisesRegex(ValueError, "127.0.0.1"):
            host.validate_runtime(config, [])
        config["services"]["web"]["ports"][0]["host_ip"] = "127.0.0.1"
        host.validate_runtime(config, [{"service": "web", "target": 80}])

    def test_compose_refuses_missing_or_ambiguous_forward(self):
        ports = [{"target": 80, "protocol": "tcp", "host_ip": "127.0.0.1"}]
        config = {"services": {"web": {"ports": ports}}}
        with self.assertRaisesRegex(ValueError, "exactly one"):
            host.validate_runtime(config, [{"service": "web", "target": 81}])
        ports.append(dict(ports[0]))
        with self.assertRaisesRegex(ValueError, "exactly one"):
            host.validate_runtime(config, [{"service": "web", "target": 80}])


class ClientTests(unittest.TestCase):
    def test_stop_can_recover_its_registered_host_after_config_is_removed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            client = Client(quiet=True)
            client.state = root / "state"
            client.cache = root / "cache"
            subprocess.run(["git", "init", "-q", root], check=True)
            with client.locked():
                key = client.key(root, "devbox")
                client.entry_path(key).write_text(
                    json.dumps({"host": "devbox", "path": str(root)})
                )
            with patch.object(client, "remote", return_value="stopped") as remote:
                with patch.object(client, "disconnect") as disconnect:
                    self.assertEqual(client.execute("stop", root), "stopped")
            remote.assert_called_once_with("devbox", "stop", key)
            disconnect.assert_called_once_with(key)

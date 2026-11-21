from hashlib import sha256
import io
import json
from pathlib import Path
import tarfile
from tempfile import TemporaryDirectory
import unittest

from remote_dev.host import install_delta, install_snapshot, verify_snapshot


def archive(files):
    stream = io.BytesIO()
    with tarfile.open(fileobj=stream, mode="w:gz") as output:
        for name, content in files.items():
            info = tarfile.TarInfo(name)
            info.size = len(content)
            output.addfile(info, io.BytesIO(content))
    stream.seek(0)
    return sha256(stream.getvalue()).hexdigest(), stream


def patch(base, deleted, files):
    metadata = json.dumps({"base": base, "deleted": deleted}).encode()
    return archive({".remote-dev.delta.json": metadata, **files})


class DeltaTests(unittest.TestCase):
    def setUp(self):
        workspace = TemporaryDirectory()
        self.addCleanup(workspace.cleanup)
        self.workspace = Path(workspace.name)
        self.base, payload = archive(
            {"unchanged": b"keep", "public/removed.txt": b"before", "edit": b"old"}
        )
        install_snapshot(self.workspace, self.base, payload)
        self.source = verify_snapshot(self.workspace, self.base)

    def test_verified_patch_preserves_base_and_replaces_deleted_directory(self):
        digest, payload = patch(
            self.base, ["public/removed.txt"], {"public": b"new file", "edit": b"new"}
        )
        self.assertTrue(install_delta(self.workspace, digest, self.base, payload))
        result = verify_snapshot(self.workspace, digest)
        self.assertEqual((result / "unchanged").read_bytes(), b"keep")
        self.assertEqual((result / "public").read_bytes(), b"new file")
        self.assertEqual((result / "edit").read_bytes(), b"new")
        self.assertEqual((self.source / "public/removed.txt").read_bytes(), b"before")
        self.assertEqual((self.source / "edit").read_bytes(), b"old")

    def test_stale_base_requires_full_transfer_and_installed_target_is_retryable(self):
        digest, payload = patch(self.base, [], {"edit": b"new"})
        self.assertFalse(install_delta(self.workspace, digest, None, payload))
        payload.seek(0)
        self.assertTrue(install_delta(self.workspace, digest, self.base, payload))
        payload.seek(0)
        self.assertTrue(install_delta(self.workspace, digest, digest, payload))

    def test_path_and_checksum_failures_do_not_install_snapshot(self):
        digest, payload = patch(self.base, [], {"../outside": b"bad"})
        with self.assertRaisesRegex(ValueError, "Unsafe workspace path"):
            install_delta(self.workspace, digest, self.base, payload)
        _, valid = patch(self.base, ["edit"], {})
        with self.assertRaisesRegex(ValueError, "digest check"):
            install_delta(self.workspace, digest, self.base, valid)
        self.assertFalse((self.workspace / "snapshots" / digest).exists())
        self.assertEqual((self.source / "edit").read_bytes(), b"old")

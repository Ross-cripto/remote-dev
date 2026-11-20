from pathlib import Path
import subprocess
import tempfile
import time
import unittest
from unittest.mock import Mock, patch

from remote_dev import watch


def create_project(directory):
    root = Path(directory) / "project"
    root.mkdir()
    subprocess.run(["git", "init", "-q", root], check=True)
    return root


def run_worker(root, state, client, on_tick, *, reconnect):
    baseline = watch.fingerprint(root)
    state.mkdir()
    key, token = "example", "current lease"
    (state / "watch-example.lease").write_text(token)
    ticks = 0
    elapsed = 0.0

    def advance(seconds):
        nonlocal ticks, elapsed
        ticks += 1
        elapsed += seconds
        on_tick(ticks, key)

    with (
        patch("remote_dev.client.Client", return_value=client),
        patch.object(watch.time, "sleep", advance),
        patch.object(watch.time, "monotonic", lambda: elapsed),
    ):
        watch._run(state, root, "fixture", key, token, baseline, reconnect)


class FingerprintTests(unittest.TestCase):
    def test_fingerprint_ignores_secrets_and_ignored_files_without_touching_git(self):
        with tempfile.TemporaryDirectory() as directory:
            root = create_project(directory)
            (root / ".gitignore").write_text("scratch.txt\n")
            source = root / "app.py"
            source.write_text("initial\n")
            subprocess.run(["git", "-C", root, "add", "app.py"], check=True)
            index = root / ".git" / "index"
            original_index = index.read_bytes()
            baseline = watch.fingerprint(root)

            (root / "scratch.txt").write_text("ignored\n")
            (root / ".env").write_text("SECRET=fixture\n")
            (root / "remote-dev.toml").write_text('host = "fixture"\n')
            self.assertEqual(watch.fingerprint(root), baseline)

            source.write_text("edited\n")
            edited = watch.fingerprint(root)
            self.assertNotEqual(edited, baseline)
            untracked = root / "new.py"
            untracked.write_text("additional source\n")
            self.assertNotEqual(watch.fingerprint(root), edited)
            untracked.unlink()
            self.assertEqual(watch.fingerprint(root), edited)
            source.unlink()
            self.assertNotEqual(watch.fingerprint(root), edited)
            self.assertEqual(index.read_bytes(), original_index)


class ScheduleTests(unittest.TestCase):
    def test_only_the_latest_settled_edit_syncs(self):
        schedule = watch.SyncSchedule("baseline")
        self.assertFalse(schedule.ready(0))
        schedule.observe("first save", 1)
        schedule.observe("second save", 1.75)
        self.assertFalse(schedule.ready(2))
        self.assertTrue(schedule.ready(3))
        schedule.succeeded(3)
        self.assertFalse(schedule.ready(50))

    def test_failed_saves_back_off_and_new_saves_reset_the_retry_budget(self):
        schedule = watch.SyncSchedule("baseline")
        schedule.observe("edit", 1)
        self.assertTrue(schedule.ready(2))
        schedule.failed(2)
        self.assertFalse(schedule.ready(2.1))
        self.assertTrue(schedule.ready(10))
        schedule.failed(10)
        self.assertFalse(schedule.ready(10.1))
        self.assertTrue(schedule.ready(20))
        schedule.failed(20)
        self.assertFalse(schedule.ready(100))

        schedule.observe("next edit", 100)
        self.assertTrue(schedule.ready(101))

    def test_lost_tunnel_retries_even_without_edits_and_stops_when_restored(self):
        schedule = watch.SyncSchedule("baseline")
        schedule.observe_connection(True, 0)
        self.assertFalse(schedule.ready(0))

        schedule.observe_connection(False, 1)
        self.assertTrue(schedule.ready(1))
        schedule.failed(1)
        schedule.observe_connection(False, 1.1)
        self.assertFalse(schedule.ready(1.1))
        schedule.failed(5)
        schedule.failed(20)
        self.assertTrue(schedule.ready(50))

        schedule.observe_connection(True, 50)
        self.assertFalse(schedule.ready(50))

    def test_reconnect_attempt_does_not_reset_backoff_until_tunnel_is_restored(self):
        schedule = watch.SyncSchedule("baseline")
        schedule.observe_connection(False, 1)
        self.assertTrue(schedule.ready(1))
        schedule.succeeded(1)
        schedule.observe_connection(False, 1.1)
        self.assertFalse(schedule.ready(1.1))


class WorkerTests(unittest.TestCase):
    def test_start_is_idempotent_and_stop_revokes_only_the_selected_worker(self):
        with tempfile.TemporaryDirectory() as directory:
            root = create_project(directory)
            state = Path(directory) / "state"
            baseline = watch.fingerprint(root)
            initial_key, other_key = "first", "second"
            try:
                watch.start(
                    state, root, "fixture", initial_key, baseline, reconnect=False
                )
                watch.start(
                    state, root, "fixture", other_key, baseline, reconnect=False
                )
                startup_deadline = time.monotonic() + 3
                while time.monotonic() < startup_deadline:
                    if watch.tail(state, initial_key):
                        break
                    time.sleep(0.02)
                self.assertIn(
                    "Watching for saved changes", watch.tail(state, initial_key)
                )
                self.assertTrue(watch.active(state, initial_key))
                self.assertTrue(watch.active(state, other_key))
                token = (state / "watch-first.lease").read_text()
                watch.start(
                    state, root, "fixture", initial_key, baseline, reconnect=False
                )
                self.assertTrue(watch.current(state, initial_key, token))

                watch.stop(state, initial_key)
                self.assertFalse(watch.active(state, initial_key))
                self.assertFalse(watch.current(state, initial_key, token))
                self.assertTrue(watch.active(state, other_key))
                watch.start(
                    state, root, "fixture", initial_key, baseline, reconnect=False
                )
                self.assertTrue(watch.active(state, initial_key))
                self.assertFalse(watch.current(state, initial_key, token))
            finally:
                watch.stop(state, initial_key)
                watch.stop(state, other_key)

    def test_unlocked_lease_from_a_dead_worker_does_not_prevent_start(self):
        with tempfile.TemporaryDirectory() as directory:
            root = create_project(directory)
            state = Path(directory) / "state"
            state.mkdir()
            stale_token = "obsolete"
            (state / "watch-example.lease").write_text(stale_token)
            self.assertFalse(watch.active(state, "example"))
            try:
                watch.start(
                    state,
                    root,
                    "fixture",
                    "example",
                    watch.fingerprint(root),
                    reconnect=False,
                )
                self.assertTrue(watch.active(state, "example"))
                self.assertFalse(watch.current(state, "example", stale_token))
            finally:
                watch.stop(state, "example")

    def test_worker_syncs_settled_saves_and_keeps_an_edit_during_sync(self):
        with tempfile.TemporaryDirectory() as directory:
            root = create_project(directory)
            source = root / "app.txt"
            source.write_text("deployed revision")
            state = Path(directory) / "state"
            client = Mock()
            synchronized = []

            def save_or_stop(ticks, key):
                if ticks == 1:
                    source.write_text("first save")
                if ticks == 2:
                    source.write_text("settled save")
                if ticks == 8:
                    source.write_text("save after stop")
                    watch.stop(state, key)

            def sync(action, directory, *, host_override, watch_token):
                self.assertEqual(
                    (action, directory, host_override), ("sync", root, "fixture")
                )
                self.assertTrue(watch.current(state, "example", watch_token))
                synchronized.append(source.read_text())
                if len(synchronized) == 1:
                    source.write_text("save during sync")

            client.execute.side_effect = sync
            client.connected.side_effect = AssertionError("Reconnect disabled")
            run_worker(root, state, client, save_or_stop, reconnect=False)
            self.assertEqual(synchronized, ["settled save", "save during sync"])
            self.assertIn("Automatic sync finished", watch.tail(state, "example"))

    def test_worker_does_not_sync_again_after_restoring_a_lost_tunnel(self):
        with tempfile.TemporaryDirectory() as directory:
            root = create_project(directory)
            state = Path(directory) / "state"
            client = Mock()
            connected = False
            attempts = 0

            def stop_later(ticks, key):
                if ticks == 12:
                    watch.stop(state, key)

            def reconnect(action, directory, *, host_override, watch_token):
                nonlocal attempts, connected
                attempts += 1
                self.assertEqual(
                    (action, directory, host_override), ("sync", root, "fixture")
                )
                self.assertTrue(watch.current(state, "example", watch_token))
                connected = True

            client.connected.side_effect = lambda key: connected
            client.execute.side_effect = reconnect
            run_worker(root, state, client, stop_later, reconnect=True)
            self.assertEqual(attempts, 1)

import json
import tempfile
import unittest
from pathlib import Path

from remote_dev.config import ConfigError
from remote_dev.detection import detect_project


class ProjectDetectionTests(unittest.TestCase):
    def setUp(self):
        temporary_directory = tempfile.TemporaryDirectory()
        self.addCleanup(temporary_directory.cleanup)
        self.root = Path(temporary_directory.name)

    def write_file(self, filename, contents):
        path = self.root / filename
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(contents, encoding="utf-8")

    def package(self, directory=".", **manifest):
        self.write_file(Path(directory) / "package.json", json.dumps(manifest))

    def test_compose_prefers_the_development_override(self):
        self.write_file("compose.yaml", "services:\n  web:\n    build: .\n")
        self.write_file("compose.dev.yaml", "services:\n  web:\n    ports: [3000]\n")
        self.write_file("compose.prod.yaml", "services:\n  web:\n    ports: [443]\n")
        self.write_file("package.json", "invalid JSON")

        project = detect_project(self.root, "lab")

        self.assertEqual(project.mode, "compose")
        self.assertEqual(project.compose_file, "compose.yaml")
        self.assertEqual(project.compose_override, "compose.dev.yaml")
        self.assertIsNone(project.generated_compose)
        self.assertTrue(project.auto_ports)

    def test_dev_only_compose_is_detected_without_a_base_file(self):
        self.write_file("docker-compose.dev.yml", "services:\n  web:\n    build: .\n")

        project = detect_project(self.root, "lab")

        self.assertEqual(project.compose_file, "docker-compose.dev.yml")
        self.assertIsNone(project.compose_override)

    def test_compose_development_overrides_are_ambiguous_if_both_exist(self):
        self.write_file("compose.yaml", "services:\n  web:\n    build: .\n")
        self.write_file("compose.dev.yaml", "services:\n  web:\n    build: .\n")
        self.write_file("compose.development.yml", "services:\n  web:\n    build: .\n")

        with self.assertRaisesRegex(
            ConfigError, "compose.dev.yaml.*compose.development.yml"
        ):
            detect_project(self.root, "lab")

    def test_production_only_compose_requires_explicit_config(self):
        self.write_file("compose.prod.yaml", "services:\n  web:\n    build: .\n")

        with self.assertRaisesRegex(ConfigError, "remote-dev.toml"):
            detect_project(self.root, "lab")

    def test_vite_uses_sampled_manager_and_nondefault_port(self):
        self.package(scripts={"dev": "vite --port 4201"})
        self.write_file("pnpm-lock.yaml", "lockfileVersion: '9.0'\n")

        project = detect_project(self.root, "lab")

        self.assertEqual(project.compose_file, ".remote-dev.generated.json")
        self.assertEqual(project.forwards[0].target, 4201)
        service = project.generated_compose["services"][project.forwards[0].service]
        self.assertIn("pnpm install --frozen-lockfile", service["command"][2])
        self.assertIn("pnpm run dev --host 0.0.0.0 --strictPort", service["command"][2])
        self.assertEqual(service["labels"]["io.remote-dev.live-source"], "/workspace")
        self.assertEqual(service["labels"]["io.remote-dev.reload"], "watch")

    def test_infrastructure_compose_and_vite_use_one_project(self):
        self.write_file(
            "compose.yaml",
            "services:\n  database:\n    image: postgres:16 # Development DB\n"
            "    ports:\n      - '5432:5432'\n"
            "  cache:\n    image: redis:7\n",
        )
        self.package(scripts={"dev": "vite"}, packageManager="pnpm@10.11.0")

        project = detect_project(self.root, "lab")

        self.assertEqual(project.compose_file, "compose.yaml")
        self.assertTrue(project.auto_ports)
        (forward,) = project.forwards
        self.assertEqual(forward.target, 5173)
        service = project.generated_compose["services"][forward.service]
        self.assertEqual(service["volumes"][0]["source"], ".")
        self.assertNotIn("database", project.generated_compose["services"])
        self.assertNotIn("cache", project.generated_compose["services"])

    def test_compose_with_a_custom_service_keeps_compose_in_control(self):
        self.write_file(
            "compose.yml",
            "services:\n  database:\n    image: postgres:16\n"
            "  frontend:\n    image: example/frontend:dev\n",
        )
        self.package(scripts={"dev": "vite"})

        project = detect_project(self.root, "lab")

        self.assertEqual(project.mode, "compose")
        self.assertIsNone(project.generated_compose)

    def test_compose_with_an_included_service_file_keeps_compose_in_control(self):
        self.write_file(
            "compose.yaml",
            '"include": [frontend.yaml]\nservices:\n  database:\n    image: postgres:16\n',
        )
        self.package(scripts={"dev": "vite"})

        project = detect_project(self.root, "lab")

        self.assertEqual(project.mode, "compose")
        self.assertIsNone(project.generated_compose)

    def test_hybrid_dependency_storage_cannot_share_a_compose_volume_name(self):
        self.write_file(
            "compose.yaml",
            "services:\n  database:\n    image: postgres:16\n"
            "    volumes:\n      - web-node-modules:/var/lib/postgresql/data\n"
            "volumes:\n  web-node-modules: {}\n",
        )
        self.package(scripts={"dev": "vite"})

        project = detect_project(self.root, "lab")

        service = project.generated_compose["services"]["web"]
        self.assertEqual(
            service["volumes"][1],
            {"type": "volume", "target": "/workspace/node_modules"},
        )
        self.assertNotIn("volumes", project.generated_compose)

    def test_hybrid_reports_generated_service_name_collision(self):
        self.write_file("compose.yaml", "services:\n  web:\n    image: postgres:16\n")
        self.package(scripts={"dev": "vite"})

        with self.assertRaisesRegex(ConfigError, "web.*remote-dev.toml"):
            detect_project(self.root, "lab")

    def test_pnpm_monorepo_installs_at_workspace_root(self):
        self.package(packageManager="pnpm@10.11.0")
        self.package("apps/frontend", scripts={"dev": "next dev"})
        self.write_file("pnpm-workspace.yaml", "packages:\n  - 'apps/*'\n")
        self.write_file("pnpm-lock.yaml", "lockfileVersion: '9.0'\n")

        project = detect_project(self.root, "lab")

        (forward,) = project.forwards
        service = project.generated_compose["services"][forward.service]
        self.assertEqual(service["working_dir"], "/workspace/apps/frontend")
        self.assertIn(
            "cd /workspace && corepack enable pnpm && pnpm install --frozen-lockfile",
            service["command"][2],
        )
        self.assertIn(
            "cd /workspace/apps/frontend && exec pnpm run dev", service["command"][2]
        )
        self.assertEqual(service["volumes"][1]["target"], "/workspace/node_modules")

    def test_pnpm_recursive_dev_runs_the_root_script_for_ready_services(self):
        self.package(scripts={"dev": "pnpm -r --parallel run dev"})
        self.package("apps/frontend", scripts={"dev": "vite --host"})
        self.package("apps/api", scripts={"dev": "next dev"})
        self.write_file("pnpm-workspace.yaml", "packages:\n  - apps/*\n")
        self.write_file("pnpm-lock.yaml", "lockfileVersion: '9.0'\n")

        project = detect_project(self.root, "lab")

        self.assertEqual({forward.target for forward in project.forwards}, {3000, 5173})
        self.assertEqual({forward.service for forward in project.forwards}, {"web"})
        self.assertIn(
            "exec pnpm run dev",
            project.generated_compose["services"]["web"]["command"][2],
        )

    def test_npm_workspace_dev_runs_the_root_script_when_one_service_is_ready(self):
        self.package(scripts={"dev": "npm run dev --workspaces"}, workspaces=["apps/*"])
        self.package("apps/api", scripts={"dev": "nest start --watch"})
        self.write_file("package-lock.json", "{}")

        project = detect_project(self.root, "lab")

        (forward,) = project.forwards
        self.assertEqual((forward.service, forward.target), ("web", 3000))
        self.assertIn(
            "npm ci", project.generated_compose["services"]["web"]["command"][2]
        )
        self.assertIn(
            "exec npm run dev",
            project.generated_compose["services"]["web"]["command"][2],
        )

    def test_unrecognized_dev_script_requests_a_commands_override(self):
        self.package(scripts={"dev": "./custom-launch"})

        with self.assertRaisesRegex(ConfigError, "dev.*remote-dev.toml.*commands"):
            detect_project(self.root, "lab")

    def test_nest_uses_the_scaffolded_start_dev_script_with_config_path(self):
        self.package(scripts={"start:dev": "nest start -w -p tsconfig.local.json"})

        project = detect_project(self.root, "lab")

        (forward,) = project.forwards
        service = project.generated_compose["services"][forward.service]
        self.assertEqual(forward.target, 3000)
        self.assertIn("exec npm run start:dev", service["command"][2])
        self.assertEqual(service["labels"]["io.remote-dev.reload"], "watch")

    def test_versioned_yarn_uses_immutable_lockfile_install(self):
        self.package(scripts={"dev": "vite"}, packageManager="yarn@4.9.1")
        self.write_file("yarn.lock", "__metadata:\n  version: 8\n")

        project = detect_project(self.root, "lab")

        service = project.generated_compose["services"]["web"]
        self.assertIn(
            "corepack enable yarn && yarn install --immutable", service["command"][2]
        )
        self.assertIn("exec yarn run dev --host", service["command"][2])

    def test_multiple_package_manager_lockfiles_require_a_declaration(self):
        self.package(scripts={"dev": "vite"})
        self.write_file("package-lock.json", "{}")
        self.write_file("pnpm-lock.yaml", "lockfileVersion: '9.0'\n")

        with self.assertRaisesRegex(ConfigError, "lockfiles.*packageManager"):
            detect_project(self.root, "lab")

    def test_single_node_server_uses_npm_ci_and_its_configured_node_major(self):
        self.package(scripts={"dev": "PORT=8080 node server.js"})
        self.write_file("package-lock.json", "{}")
        self.write_file(".nvmrc", "v22.14.0\n")

        project = detect_project(self.root, "lab")

        (forward,) = project.forwards
        service = project.generated_compose["services"][forward.service]
        self.assertEqual(forward.target, 8080)
        self.assertEqual(service["image"], "node:22-bookworm-slim")
        self.assertIn("npm ci", service["command"][2])
        self.assertEqual(service["labels"]["io.remote-dev.reload"], "restart")

    def test_invalid_ssh_alias_is_rejected_before_detection(self):
        with self.assertRaisesRegex(ConfigError, "SSH host alias"):
            detect_project(self.root, "-bad")


if __name__ == "__main__":
    unittest.main()

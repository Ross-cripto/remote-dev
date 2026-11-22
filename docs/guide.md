# Remote Dev user guide

[Back to Remote Dev](../README.md)

## Install a release

Download the archive for your client machine and `SHA256SUMS` from the same [GitHub release](https://github.com/Ross-cripto/remote-dev/releases). Use `darwin-arm64` on Apple Silicon, `darwin-amd64` on Intel Macs, `linux-amd64` on x86-64 Linux, or `linux-arm64` on ARM64 Linux.

From the download directory, set `archive` to the filename you downloaded.

```sh
archive=remote-dev-v0.3.0-rc.1-darwin-arm64.tar.gz
awk -v archive="$archive" '$2 == archive' SHA256SUMS | shasum -a 256 -c
```

On Linux, use `sha256sum -c` in place of `shasum -a 256 -c` if needed. Once verification prints `OK`, install the executable.

```sh
tar -xzf "$archive" remote-dev
mkdir -p "$HOME/.local/bin"
install -m 755 remote-dev "$HOME/.local/bin/remote-dev"
export PATH="$HOME/.local/bin:$PATH"
remote-dev --version
```

The binary includes both Linux helpers. Add the PATH line to your shell configuration to keep the command available in new terminals.

## Install from a checkout

Build the bundled executable with Go 1.26 or newer. Make sure `$HOME/.local/bin` is on your PATH or choose an installation directory already on it.

```sh
git clone https://github.com/Ross-cripto/remote-dev.git
cd remote-dev
./scripts/build-native.sh "$HOME/.local/bin/remote-dev"
export PATH="$HOME/.local/bin:$PATH"
remote-dev --version
```

The executable includes Linux helpers for amd64 and arm64. You can choose a separate name such as `remote-dev-go` if an existing tool already uses `remote-dev`.

## Connect and check a server

The client needs Git and OpenSSH. The Linux server needs Docker Engine and the Compose v2 plugin. Your SSH account must be able to use Docker and open SSH forwards. Remote Dev selects the correct bundled Go helper for your server, verifies its checksum and installs it in your SSH user's data directory.

```sh
remote-dev connect devbox
remote-dev doctor
```

Use your existing OpenSSH alias or a target like `alex@devbox.example.com`. To select a specific identity, pass `--identity ~/.ssh/id_ed25519`. Use `--name devbox` to save a personal alias for a literal target. OpenSSH handles authentication and host identity verification, and the private key stays on your laptop. Remote Dev remembers a successful connection as the default.

On Debian or Ubuntu, `remote-dev connect devbox --prepare` can propose installing missing prerequisites available in the server's configured APT repositories. It simulates the package operation and asks for an interactive `yes` before applying it. Installation requires the corresponding system permissions. On other distributions, use your distribution's package instructions, then rerun `remote-dev doctor`.

## Try the Compose demo

From a [checkout of this repository](#install-from-a-checkout), after you install Remote Dev and connect a server, copy the Compose fixture into a temporary Git project.

```sh
demo=$(mktemp -d)
cp examples/compose/compose.yaml "$demo/"
cp -R examples/compose/public "$demo/"
cd "$demo"
git init
remote-dev up
```

Open the URL printed by `up`. Edit the heading in `public/index.html`, save, and refresh the page at the same URL. The example serves static HTML. A framework with hot reload can refresh through its own development server.

Run `remote-dev status` to inspect the service and `remote-dev stop` to stop the demo.

## Daily commands

Run project commands from anywhere inside the relevant Git worktree.

| Command | What it does |
| --- | --- |
| `remote-dev up` | Detects or reads the setup, starts the services, opens the SSH forwards, and watches for saved changes. |
| `remote-dev up --no-watch` | Starts the workspace with manual sync. |
| `remote-dev test [--service SERVICE]` | Runs the current worktree's tests on Linux and streams the output. |
| `remote-dev sync` | Sends the current worktree source and refreshes running services where needed. |
| `remote-dev status` | Shows container state, local URLs, and recent watcher status. |
| `remote-dev logs` | Reads recent service logs. |
| `remote-dev open` | Opens the first forwarded URL in your browser. |
| `remote-dev stop` | Stops the selected worktree's services, watcher, and tunnel. |
| `remote-dev migrate` | Adopts a worktree from the Python beta or refreshes the Go preview's watcher. |
| `remote-dev list` | Lists worktrees previously used by this client. |
| `remote-dev` | Opens a terminal panel for the current and previously used worktrees. |

To run another branch at the same time, open its Git worktree in a second terminal and run `remote-dev up` there. Each worktree uses its own local ports and Compose project. The laptop-side watcher survives closing the terminal and runs until you stop its workspace. It tries to restore dropped tunnels and reuses the previous local port when available.

## Run tests on Linux

From anywhere inside your Git worktree, run:

```sh
remote-dev test
```

- **Compose** takes precedence when a Compose file is detected. Give your suite a service named `test` or `tests`, or use `remote-dev test --service integration` to choose a service by name.
- **Node.js** uses the root `package.json` test script and detects npm, pnpm, or Yarn. Automatic Node tests set `CI=true`.
- **Explicit configuration** can define a `test` or `tests` service in `remote-dev.toml`. In Node mode, set its `script` to your test script. An explicit `--service` selects another configured service.

Each run sends the current saved files and creates a temporary Compose project with its own network and volumes. Existing previews can keep running. Test output appears as it arrives, and the command returns the test process's exit code. Press Ctrl+C to cancel the run. Compose jobs use project-scoped resources and do not publish service ports.

## Configure a project explicitly

Start with automatic detection. If your project has ambiguous development scripts, custom startup commands, or port requirements, add `remote-dev.toml` to the worktree.

```sh
remote-dev init --host devbox --mode commands
```

Edit the generated file before running `up`. The [Compose](../examples/compose/remote-dev.toml), [commands](../examples/commands/remote-dev.toml), and [Node](../examples/node/remote-dev.toml) examples show the available modes. For explicit Compose forwarding, publish the remote host ports on `127.0.0.1`. Use `local = "auto"` to let Remote Dev choose the laptop port, or name the port you want.

## Source and sync

- Your local Git worktree is the source of truth. Remote Dev sends the current contents of Git-tracked and non-ignored untracked files, including uncommitted changes saved to disk. It leaves your Git index in place and omits Git metadata.
- Standard dotenv credentials, common key filenames, and a few credential directories are excluded by name. Keep other sensitive untracked files ignored and store secrets outside tracked source. Use a trusted Linux host and trusted project code.
- The first start sends a compressed snapshot. Subsequent saves send edited files and deletions. If the remote base changed, the client can send a full snapshot to recover. The Linux helper verifies each payload before applying it. Some code, dependency, or configuration changes require a service restart or rebuild.
- Write generated output to container storage or a named volume. Changing synchronized source directly on the server can cause a conflict. Source symlinks and Git submodules are unsupported in this beta.

## Troubleshooting

| What you see | What to check |
| --- | --- |
| SSH connects but Docker fails | Run `remote-dev doctor` and check that the same SSH account can run `docker info` and `docker compose version` on the server. |
| The project can't be detected | Read the file or script named in the error and use [explicit configuration](#configure-a-project-explicitly) for unusual setups. |
| A service is up but the URL fails | Check `remote-dev status` and `remote-dev logs`. The application must listen on its container interface and the forwarded port. |
| The page doesn't update | Confirm the file is tracked or unignored, check watcher status, and try `remote-dev sync`. Refresh the browser for frameworks without hot reload. |

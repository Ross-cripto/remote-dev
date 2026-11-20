# Remote Dev

Keep editing in your local Git worktree. Run your development services on a Linux box you control, and open them through SSH on `localhost`. Save a file and see the new version from the same URL.

Each worktree uses a separate Compose project and tunnel. Remote Dev detects the development setup from the repository. An explicit configuration is available for projects with custom startup commands.

## Install and connect

The client uses Python 3.11+, Git, and OpenSSH. The Linux host needs Python 3.11+ and a Docker Engine with the Compose plugin accessible to your SSH account.

```sh
git clone https://github.com/Ross-cripto/remote-dev.git
cd remote-dev
python3 -m venv .venv
. .venv/bin/activate
python -m pip install .
remote-dev connect devbox
```

`devbox` can be an existing OpenSSH alias. You can also use `remote-dev connect user@server --identity ~/.ssh/id_ed25519` to reference a local key. OpenSSH handles authentication and first-use server verification. The private key stays on the client. A successful connection is saved as the default and can be reused across repositories.

If host prerequisites are missing, rerun `remote-dev connect devbox --prepare`. On Debian and Ubuntu with compatible packages in the configured APT repositories, Remote Dev simulates installation and presents the package changes. Enter `yes` at the prompt to authorize them. The SSH account needs the appropriate access for installation. Run `remote-dev doctor` to check the saved server again.

## Open a project

Run this from a Git worktree after connecting your server.

```sh
remote-dev up
```

For a quick demo from this checkout, create a temporary Git project with the example's Compose and source files.

```sh
demo=$(mktemp -d)
cp examples/compose/compose.yaml "$demo/"
cp -R examples/compose/public "$demo/"
cd "$demo"
git init
git add .
remote-dev up
```

Open the URL printed by `up`. Edit `public/index.html` and save it. The new content is served through the same URL. An app with a hot reload feature, such as Vite, can update the open page directly. Run `remote-dev stop` when done. Launch `remote-dev` without arguments for the terminal panel.

### Detection

| Project | What Remote Dev uses |
| --- | --- |
| Compose | A root Compose file and supported development overrides or profiles. Detected application ports use loopback addresses and available host ports. |
| Node | Recognizable development scripts, package manager and lockfile. Supports common Vite, Next.js and NestJS launch commands and supported npm, pnpm and Yarn workspaces. |
| Mixed | A recognizable Node app can run alongside a simple infrastructure-only Compose project on its default network. |

The output identifies the files used to detect your project. Projects with ambiguous application scripts, custom commands, and complex layouts can still use an explicit `remote-dev.toml`. Try `remote-dev init --host devbox --mode commands` to create a template, or inspect the [Compose](examples/compose/), [commands](examples/commands/), and [Node](examples/node/) examples. If you explicitly configure forwarded Compose ports, publish them on `127.0.0.1` on the host. Set `local = "auto"` or choose a local port in the project configuration.

## Daily use

| Command | Behavior |
| --- | --- |
| `remote-dev connect [user@host]` | Check and remember the server |
| `remote-dev up` | Open a workspace and sync saved changes automatically |
| `remote-dev up --no-watch` | Open a workspace with manual synchronization |
| `remote-dev sync` | Sync the current worktree now |
| `remote-dev status` | Show services, tunnel URLs, and recent sync status |
| `remote-dev logs` | Read recent service output |
| `remote-dev stop` | Stop the selected worktree, watcher, and tunnel |
| `remote-dev list` | List worktrees used by this client |
| `remote-dev` | Open the terminal panel |

## Beta behavior

- Saving syncs from the local worktree to the server. Ordinary edits update the running service where possible. Depending on the project's configuration, a service may instead restart or rebuild. Runtime configuration and dependency changes may recreate affected containers.
- The client still sends a compressed snapshot when it syncs. The host applies changed files and retains the current verified revision for recovery. The laptop-side watcher runs until the workspace stops. If the tunnel drops, the watcher tries to restore it and reuses the local port when available.
- The source includes the current bytes of Git-tracked and non-ignored untracked files. Common credential filenames are excluded. Use `.gitignore` for additional private files. Keep secrets in your existing host setup or secret manager. The local Git index stays intact, and the server receives a source tree without Git metadata.
- Use your own trusted projects and servers because service commands run there. Store generated files in container storage or named volumes. Remote changes to managed source files are reported as conflicts. Git submodules and source symlinks are unsupported.

Run focused checks with `python3 -m unittest discover -s tests`.

[MIT license](LICENSE)

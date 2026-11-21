# Upgrade to the Go version

The Go executable includes its Linux helper. When you connect, Remote Dev checks SSH and Docker, selects a helper for the server's architecture and installs the verified binary for the SSH user.

## Adopt a worktree from the Python beta

From the same Git worktree, run these commands with the new executable.

```sh
remote-dev connect
remote-dev migrate
remote-dev status
```

The first command checks the saved default SSH connection. You can instead pass `--name devbox` to select a saved connection. If several Python sessions exist for the same worktree, use `remote-dev migrate --host devbox` to select the intended one.

Migration adopts the existing server workspace identity and forwards. A live tunnel can keep serving the same local URL and Docker project while the Go watcher takes over. The first Go sync may send a full snapshot to establish its baseline. Subsequent edits send incremental updates. Continue managing the adopted worktree with the Go command.

Run `remote-dev migrate` from a Go preview worktree to install the native helper, carry its existing workspace record forward and refresh its watcher.

## Check after switching

- Review `remote-dev status` and open the reported URL.
- Save a file, then confirm the preview updates.
- Keep your existing `remote-dev.toml`, if the project uses one. Automatic detection supports Compose and recognized Node projects.

The Go client uses its own client state directory. The Python beta's connection and worktree records are read for migration. Both helpers recognize the existing Linux workspace directory and deployment format.

# Remote Dev

Remote Dev runs development services for local Git worktrees on a personal Linux server over SSH.

The CLI and terminal panel use Python 3.11+ and the standard library. The host uses OpenSSH, Python 3.11+ and Docker Compose. Run `python3 -m remote_dev --help` or install the command in a virtual environment with `python3 -m pip install -e .`. Run focused checks with `python3 -m unittest discover -s tests`.

Keep examples synthetic and verify them in separate Compose projects. Preserve local Git state and scope host actions to the selected workspace identity.

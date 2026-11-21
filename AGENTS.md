# Remote Dev

Remote Dev runs development services for local Git worktrees on a personal Linux server over SSH.

The client, terminal panel and Linux helper use Go. Build the complete client with `./scripts/build-native.sh`. Run focused checks with Go tests and vet for the affected packages.

The Python beta is the upgrade reference. Maintain readable legacy snapshot formats and use `python3 -m unittest discover -s tests` to check compatibility when the migration or source protocol changes.

Keep examples synthetic and verify them in separate Compose projects. Preserve local Git state and scope host actions to the selected workspace identity.

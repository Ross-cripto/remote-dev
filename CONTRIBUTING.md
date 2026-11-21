# Contributing to Remote Dev

A good first contribution is a small example that Remote Dev should detect or a reproducible issue from a project you already run. Please use synthetic project files and remove hostnames, credentials, and private paths from output before sharing it.

## Work on the CLI

```sh
git clone https://github.com/Ross-cripto/remote-dev.git
cd remote-dev
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -e .
remote-dev --help
python -m unittest discover -s tests
```

The automated suite can run without your Linux server. If a change affects SSH, port forwarding, or service lifecycle, also try the [Compose demo](docs/guide.md#try-the-compose-demo) on a disposable worktree and describe what you observed.

Put new examples under [examples/](examples/). Keep project discovery, Git staging, SSH identity, and service scope explicit when changing the client. Include the triggering project shape and a test or reproduction for behavioral changes.

For artwork, use the [brand assets](docs/brand.md) and capture real product output for examples.

## Report an issue

Use the [issue forms](https://github.com/Ross-cripto/remote-dev/issues/new/choose) for bug reports or ideas. Include your client OS, server distribution, Remote Dev version, and whether the project uses Compose, Node, or explicit configuration. Use the [security policy](SECURITY.md) for sensitive reports.

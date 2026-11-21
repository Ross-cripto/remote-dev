#!/bin/sh
set -eu

remote_dev_source=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
remote_dev_go=${REMOTE_DEV_GO:-go}
remote_dev_host_os=$("$remote_dev_go" env GOHOSTOS)
remote_dev_host_arch=$("$remote_dev_go" env GOHOSTARCH)
remote_dev_target_os=${GOOS:-$remote_dev_host_os}
remote_dev_target_arch=${GOARCH:-$remote_dev_host_arch}
remote_dev_output=${1:-"$remote_dev_source/dist/remote-dev-$remote_dev_target_os-$remote_dev_target_arch"}
case "$remote_dev_output" in
    /*) ;;
    *) remote_dev_output="$PWD/$remote_dev_output" ;;
esac
remote_dev_stage=$(mktemp -d "${TMPDIR:-/tmp}/remote-dev-build.XXXXXX")
trap 'rm -rf -- "$remote_dev_stage"' EXIT HUP INT TERM

cp "$remote_dev_source/go.mod" "$remote_dev_source/go.sum" "$remote_dev_stage/"
cp -R "$remote_dev_source/cmd" "$remote_dev_source/internal" "$remote_dev_stage/"
cd "$remote_dev_stage"

for remote_dev_helper_arch in amd64 arm64; do
    CGO_ENABLED=0 GOOS=linux GOARCH="$remote_dev_helper_arch" "$remote_dev_go" build -trimpath -ldflags='-s -w' -o helper ./cmd/remote-dev-helper
    remote_dev_asset="internal/payload/assets/linux-$remote_dev_helper_arch"
    gzip -n -c helper > "$remote_dev_asset.gz"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum helper | cut -d ' ' -f 1 > "$remote_dev_asset.sha256"
    else
        shasum -a 256 helper | cut -d ' ' -f 1 > "$remote_dev_asset.sha256"
    fi
done
mkdir -p "$(dirname -- "$remote_dev_output")"
CGO_ENABLED=0 GOOS="$remote_dev_target_os" GOARCH="$remote_dev_target_arch" "$remote_dev_go" build -trimpath -ldflags='-s -w' -o "$remote_dev_output" ./cmd/remote-dev
printf '%s\n' "$remote_dev_output"

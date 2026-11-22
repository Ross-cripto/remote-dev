#!/bin/sh
set -eu

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
    printf 'Usage: %s v<VERSION> [OUTPUT_DIRECTORY]\n' "$0" >&2
    exit 1
fi

release_tag=$1
source_directory=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
build_go=${REMOTE_DEV_GO:-go}
target_os=$("$build_go" env GOHOSTOS)
target_arch=$("$build_go" env GOHOSTARCH)
case "$target_os/$target_arch" in
    darwin/arm64|darwin/amd64|linux/amd64|linux/arm64) ;;
    *) printf 'Unsupported release platform %s/%s.\n' "$target_os" "$target_arch" >&2; exit 1 ;;
esac

output_directory=${2:-"$source_directory/dist"}
mkdir -p -- "$output_directory"
output_directory=$(CDPATH='' cd -- "$output_directory" && pwd)
package_stage=$(mktemp -d "$output_directory/.remote-dev-package.XXXXXX")
trap 'rm -rf -- "$package_stage"' EXIT HUP INT TERM

GOOS="$target_os" GOARCH="$target_arch" "$source_directory/scripts/build-native.sh" "$package_stage/remote-dev" >/dev/null
built_version=$("$package_stage/remote-dev" --version)
if [ "v$built_version" != "$release_tag" ]; then
    printf 'Tag %s and remote-dev version %s differ.\n' "$release_tag" "$built_version" >&2
    exit 1
fi

archive="remote-dev-$release_tag-$target_os-$target_arch.tar.gz"
cp "$source_directory/LICENSE" "$package_stage/LICENSE"
(
    cd "$package_stage"
    COPYFILE_DISABLE=1 tar -czf "$archive" remote-dev LICENSE
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$archive" > SHA256SUMS
    else
        shasum -a 256 "$archive" > SHA256SUMS
    fi
)
mv -f -- "$package_stage/$archive" "$package_stage/SHA256SUMS" "$output_directory/"
printf '%s\n' "$output_directory/$archive"

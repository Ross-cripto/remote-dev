package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/Ross-cripto/remote-dev/internal/payload"
)

const helperMissing = 43
const installTimeout = 90 * time.Second

const installHelperScript = `set -eu
umask 077
destination=${XDG_DATA_HOME:-"$HOME/.local/share"}/remote-dev/helpers/$1
mkdir -p -- "$destination"
incoming=$(mktemp "$destination/.incoming.XXXXXXXX")
trap 'rm -f -- "$incoming"' EXIT
cat > "$incoming"
hash=$(sha256sum < "$incoming")
if [ "${hash%% *}" != "$1" ]; then
    printf '%s\n' "Remote Dev helper checksum mismatch." >&2
    exit 1
fi
chmod 700 "$incoming"
mv -f -- "$incoming" "$destination/remote-dev-helper"
`

const runHelperScript = `set -eu
helper=${XDG_DATA_HOME:-"$HOME/.local/share"}/remote-dev/helpers/$1/remote-dev-helper
shift
[ -x "$helper" ] || exit 43
exec "$helper" "$@"
`

const serverProbe = `set -eu
export LC_ALL=C
printf 'kernel\t%s\n' "$(uname -s)"
printf 'machine\t%s\n' "$(uname -m)"
ID=
if [ -r /etc/os-release ]; then . /etc/os-release; fi
printf 'distro\t%s\n' "$ID"
if ! command -v docker >/dev/null 2>&1; then
    printf 'docker\tmissing\n'
    exit 0
fi
if engine=$(docker info --format '{{.OSType}}' 2>&1); then
    printf 'docker\t%s\n' "$engine"
else
    printf 'docker\tinaccessible\n'
    printf '%s\n' "$engine" >&2
fi
if version=$(docker compose version --short 2>/dev/null); then
    printf 'compose\t%s\n' "$version"
fi
`

type hostFacts struct {
	Architecture string
	Distro       string
	Docker       string
	Compose      string
}

func serverArchitecture(machine string) (string, error) {
	switch machine {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("no bundled Linux helper for the host architecture %q", machine)
	}
}

func (facts hostFacts) ready() error {
	switch facts.Docker {
	case "linux":
	case "missing":
		return errors.New("Docker Engine is missing. Use connect --prepare on a supported Debian or Ubuntu host")
	default:
		return errors.New("the SSH account needs access to a Linux Docker Engine. Check docker info on the host")
	}
	if !meetsVersion(facts.Compose, 2, 0) {
		return errors.New("Docker Compose v2 or newer is required. Use connect --prepare on a supported Debian or Ubuntu host")
	}
	return nil
}

func (connection Connection) probe(ctx context.Context, interactive bool) (hostFacts, error) {
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	output, err := connection.command(ctx, nil, shellCommand("sh", "-c", serverProbe), interactive)
	if err != nil {
		return hostFacts{}, err
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		if field, value, ok := strings.Cut(line, "\t"); ok {
			values[field] = strings.TrimSpace(value)
		}
	}
	if values["kernel"] != "Linux" {
		return hostFacts{}, errors.New("Remote Dev requires a Linux server")
	}
	architecture, err := serverArchitecture(values["machine"])
	if err != nil {
		return hostFacts{}, err
	}
	return hostFacts{Architecture: architecture, Distro: values["distro"], Docker: values["docker"], Compose: values["compose"]}, nil
}

func (connection Connection) installHelper(ctx context.Context, digest string) error {
	compressed, err := payload.Open(connection.Architecture)
	if err != nil {
		return err
	}
	defer compressed.Close()
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	_, err = connection.command(ctx, compressed, shellCommand("sh", "-c", installHelperScript, "remote-dev", digest), false)
	if err != nil {
		return fmt.Errorf("install Go helper: %w", err)
	}
	return nil
}

func (connection Connection) invokeHelper(ctx context.Context, input io.Reader, arguments ...string) ([]byte, error) {
	if connection.Architecture == "" {
		facts, err := connection.probe(ctx, false)
		if err != nil {
			return nil, err
		}
		if err := facts.ready(); err != nil {
			return nil, err
		}
		connection.Architecture = facts.Architecture
	}

	digest, err := payload.Digest(connection.Architecture)
	if err != nil {
		return nil, err
	}
	command := shellCommand(append([]string{"sh", "-c", runHelperScript, "remote-dev", digest}, arguments...)...)
	output, err := connection.command(ctx, input, command, false)
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != helperMissing {
		return output, err
	}
	if input != nil {
		source, seekable := input.(io.Seeker)
		if !seekable {
			return nil, errors.New("install the missing Go helper with remote-dev connect before streaming input")
		}
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
	}
	if err := connection.installHelper(ctx, digest); err != nil {
		return nil, err
	}
	return connection.command(ctx, input, command, false)
}

func (connection Connection) prepared(ctx context.Context, facts hostFacts) (Connection, string, error) {
	connection.Architecture = facts.Architecture
	response, err := connection.invokeHelper(ctx, nil, "doctor")
	if err != nil {
		return Connection{}, "", err
	}
	return connection, strings.TrimSpace(string(response)), nil
}

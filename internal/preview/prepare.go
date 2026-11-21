package preview

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"golang.org/x/term"
)

var packagePolicy = regexp.MustCompile(`(?m)^([a-z0-9.-]+):\n +Installed: (.+)\n +Candidate: (.+)$`)
var vendorPackages = []string{"docker-ce", "docker-ce-cli", "containerd.io", "docker-buildx-plugin", "docker-compose-plugin"}
var packageCandidates = []string{"docker.io", "docker-compose", "docker-compose-v2", "docker-ce", "docker-ce-cli", "containerd.io", "docker-buildx-plugin", "docker-compose-plugin"}

const aptProbe = `set -eu
export LC_ALL=C
printf 'uid\t%s\n' "$(id -u)"
if command -v sudo >/dev/null; then printf 'sudo\tyes\n'; fi
apt-cache policy "$@"
`

type packageVersion struct {
	installed string
	candidate string
}

func chooseAPT(distro string, hasEngine bool, packages map[string]packageVersion) ([]string, error) {
	var distroCompose string
	switch distro {
	case "debian":
		distroCompose = "docker-compose"
	case "ubuntu":
		distroCompose = "docker-compose-v2"
	default:
		return nil, fmt.Errorf("automatic preparation uses the configured APT repositories on Debian and Ubuntu. Set up Docker manually on %s", distro)
	}
	distroOK := meetsVersion(packages[distroCompose].candidate, 2, 0)
	vendorOK := meetsVersion(packages["docker-compose-plugin"].candidate, 2, 0)
	if hasEngine {
		switch {
		case packages["docker-ce-cli"].installed != "" && vendorOK:
			return []string{"docker-compose-plugin"}, nil
		case packages["docker.io"].installed != "" && distroOK:
			return []string{distroCompose}, nil
		default:
			return nil, errors.New("no compatible Compose v2 package for the installed Docker Engine in the configured APT repositories")
		}
	}
	if distroOK && packages["docker.io"].candidate != "" {
		return []string{"docker.io", distroCompose}, nil
	}
	if vendorOK {
		for _, name := range vendorPackages {
			if packages[name].candidate == "" {
				return nil, fmt.Errorf("configured APT repositories are missing %s", name)
			}
		}
		return vendorPackages, nil
	}
	return nil, errors.New("configured APT repositories lack a compatible Docker Engine and Compose v2")
}

func availablePackages(output string) map[string]packageVersion {
	packages := map[string]packageVersion{}
	for _, match := range packagePolicy.FindAllStringSubmatch(output, -1) {
		installed, candidate := match[2], match[3]
		if installed == "(none)" {
			installed = ""
		}
		if candidate == "(none)" {
			candidate = ""
		}
		packages[match[1]] = packageVersion{installed: installed, candidate: candidate}
	}
	return packages
}

func aptPrivilege(output string) ([]string, error) {
	var sudo, knownUser bool
	for _, line := range strings.Split(output, "\n") {
		if line == "uid\t0" {
			return nil, nil
		}
		if strings.HasPrefix(line, "uid\t") {
			knownUser = true
		}
		if line == "sudo\tyes" {
			sudo = true
		}
	}
	if knownUser && sudo {
		return []string{"sudo", "--"}, nil
	}
	return nil, errors.New("automatic preparation requires root or sudo access on the Linux host")
}

func confirmPreparation(interactive bool, input io.Reader) error {
	if !interactive {
		return errors.New("run connect --prepare in a terminal to authorize system package installation")
	}
	reply, err := bufio.NewReader(input).ReadString('\n')
	if err != nil || !strings.EqualFold(strings.TrimSpace(reply), "yes") {
		return errors.New("server preparation canceled")
	}
	return nil
}

func (connection Connection) prepare(ctx context.Context, writer io.Writer, facts hostFacts) (hostFacts, error) {
	if facts.Distro != "debian" && facts.Distro != "ubuntu" {
		return hostFacts{}, fmt.Errorf("automatic preparation uses configured Debian or Ubuntu APT repositories. Install the missing packages for %s", facts.Distro)
	}
	if facts.Docker != "missing" && facts.Docker != "linux" {
		return hostFacts{}, errors.New("resolve Docker Engine access before preparing packages")
	}
	arguments := append([]string{"sh", "-c", aptProbe, "remote-dev"}, packageCandidates...)
	probe, err := connection.command(ctx, nil, shellCommand(arguments...), false)
	if err != nil {
		return hostFacts{}, err
	}
	privilege, err := aptPrivilege(string(probe))
	if err != nil {
		return hostFacts{}, err
	}
	packages, err := chooseAPT(facts.Distro, facts.Docker == "linux", availablePackages(string(probe)))
	if err != nil {
		return hostFacts{}, err
	}
	simulate := append([]string{"apt-get", "--simulate", "--no-remove", "install"}, packages...)
	proposal, err := connection.command(ctx, nil, shellCommand(simulate...), false)
	if err != nil {
		return hostFacts{}, fmt.Errorf("APT could not plan the installation: %w", err)
	}
	install := append(append(privilege, "apt-get", "--no-remove", "--yes", "install"), packages...)
	fmt.Fprintf(writer, "Proposed package changes on %s\n%s\n\n%s\n", connection.Target, strings.Join(install, " "), strings.TrimSpace(string(proposal)))
	output, isFile := writer.(*os.File)
	interactive := isFile && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(output.Fd()))
	if interactive {
		fmt.Fprint(writer, "Type yes to authorize these package changes. ")
	}
	if err := confirmPreparation(interactive, os.Stdin); err != nil {
		return hostFacts{}, err
	}
	sshArguments := append(connection.options(false), "-tt", "-o", "ClearAllForwardings=yes", "-o", "ControlPath=none",
		"--", connection.Target, shellCommand(install...))
	command := exec.CommandContext(ctx, "ssh", sshArguments...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, writer, os.Stderr
	if err := command.Run(); err != nil {
		return hostFacts{}, fmt.Errorf("APT installation failed: %w", err)
	}
	facts, err = connection.probe(ctx, false)
	if err != nil {
		return hostFacts{}, err
	}
	return facts, facts.ready()
}

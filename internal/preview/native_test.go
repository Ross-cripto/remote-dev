package preview

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreparePrintsSimulatedPlanWithoutInstallingInAPipe(t *testing.T) {
	tools := t.TempDir()
	log := filepath.Join(tools, "requests")
	mockSSH := `#!/bin/sh
for argument do command=$argument; done
printf '%s\n' "$command" >> "$REMOTE_DEV_TEST_APT_LOG"
case "$command" in
    *"uname -s"*) printf 'kernel\tLinux\nmachine\tx86_64\ndistro\tubuntu\ndocker\tmissing\n' ;;
    *"apt-cache policy"*) printf 'uid\t0\ndocker.io:\n  Installed: (none)\n  Candidate: 28.1\ndocker-compose-v2:\n  Installed: (none)\n  Candidate: 2.39\n' ;;
    *"--simulate"*) printf 'The following NEW packages will be installed: docker.io docker-compose-v2\n' ;;
    *) printf 'unexpected SSH action\n' >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(tools, "ssh"), []byte(mockSSH), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools)
	t.Setenv("REMOTE_DEV_TEST_APT_LOG", log)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var output bytes.Buffer
	err := Run(t.Context(), &output, []string{"connect", "devbox", "--prepare"})
	if err == nil || !strings.Contains(err.Error(), "authorize system package installation") {
		t.Fatalf("expected a terminal confirmation error, got %v", err)
	}
	if !strings.Contains(output.String(), "The following NEW packages will be installed") {
		t.Fatalf("no simulated plan: %s", output.String())
	}
	requests, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(requests), "'--yes'") {
		t.Fatal("attempted package installation without authorization")
	}
}

func TestNativeHelperInstallRejectsACorruptPayload(t *testing.T) {
	data := t.TempDir()
	payload := []byte("#!/bin/sh\nprintf verified\\\\n\n")
	hash := sha256.Sum256(payload)
	digest := hex.EncodeToString(hash[:])
	install := func(content string) error {
		command := exec.Command("sh", "-c", installHelperScript, "remote-dev", digest)
		command.Env = append(os.Environ(), "XDG_DATA_HOME="+data)
		command.Stdin = strings.NewReader(content)
		return command.Run()
	}
	if err := install("damaged"); err == nil {
		t.Fatal("accepted a corrupt helper payload")
	}
	path := filepath.Join(data, "remote-dev", "helpers", digest, "remote-dev-helper")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed installation published a binary: %v", err)
	}
	if err := install(string(payload)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("unexpected helper permissions: %v", info.Mode())
	}
	if err := install("incorrect update"); err == nil {
		t.Fatal("accepted a corrupt replacement")
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != string(payload) {
		t.Fatalf("changed existing helper: %q, %v", content, err)
	}
}

func TestServerArchitectureAcceptsKnownLinuxPlatforms(t *testing.T) {
	for machine, want := range map[string]string{"x86_64": "amd64", "aarch64": "arm64"} {
		if got, err := serverArchitecture(machine); err != nil || got != want {
			t.Fatalf("serverArchitecture(%q) = %q, %v", machine, got, err)
		}
	}
	if _, err := serverArchitecture("riscv64"); err == nil {
		t.Fatal("accepted a platform without a bundled helper")
	}
}

func TestAPTPackagePlanPreservesExistingEngine(t *testing.T) {
	installed := map[string]packageVersion{
		"docker.io":         {installed: "28.1", candidate: "28.1"},
		"docker-compose-v2": {candidate: "2.39"},
	}
	got, err := chooseAPT("ubuntu", true, installed)
	if err != nil || len(got) != 1 || got[0] != "docker-compose-v2" {
		t.Fatalf("expected just the Compose plugin, found %v, %v", got, err)
	}
	if _, err := chooseAPT("debian", true, installed); err == nil {
		t.Fatal("accepted unavailable candidate")
	}
}

func TestPrepareConfirmationRejectsNoninteractiveAndDecline(t *testing.T) {
	if err := confirmPreparation(false, strings.NewReader("yes\n")); err == nil {
		t.Fatal("accepted installation from a piped response")
	}
	if err := confirmPreparation(true, strings.NewReader("no\n")); err == nil {
		t.Fatal("accepted a declined proposal")
	}
	if err := confirmPreparation(true, strings.NewReader("yes\n")); err != nil {
		t.Fatal(err)
	}
}

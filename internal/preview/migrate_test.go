package preview

import (
	"maps"
	"path/filepath"
	"testing"
)

func TestLegacyWorkspaceSelectionPreservesKeyAndPorts(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	key := "0123456789abcdef01234567"
	old := legacyWorkspace{
		Root: root, Host: "lab", Mode: "compose",
		LocalPorts: map[string]int{"web": 8180}, RemotePorts: map[string]int{"web": 41683},
	}
	if err := writeJSON(filepath.Join(state, "workspace-"+key+".json"), old); err != nil {
		t.Fatal(err)
	}
	record, err := findLegacyWorkspace(state, root, "")
	if err != nil {
		t.Fatal(err)
	}
	converted := record.native(Connection{Target: "lab", Architecture: "amd64"})
	if converted.Key != key || !converted.LegacyTunnel || !maps.Equal(converted.LocalPorts, old.LocalPorts) ||
		!maps.Equal(converted.RemotePorts, old.RemotePorts) {
		t.Fatalf("did not preserve active identity and ports: %#v", converted)
	}

	old.Host = "second"
	if err := writeJSON(filepath.Join(state, "workspace-abcdef0123456789abcdef01.json"), old); err != nil {
		t.Fatal(err)
	}
	if _, err := findLegacyWorkspace(state, root, ""); err == nil {
		t.Fatal("ambiguously selected one of two legacy sessions")
	}
	record, err = findLegacyWorkspace(state, root, "lab")
	if err != nil || record.Key != key {
		t.Fatalf("failed to select the requested old session: %#v, %v", record, err)
	}
}

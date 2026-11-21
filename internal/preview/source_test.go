package preview

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGeneratedConfigTravelsWithSnapshotAndWithItsOwnDelta(t *testing.T) {
	root := gitFixture(t)
	writeFixture(t, root, "app", "unchanged")
	first := []byte(`{"services":{"first":{}}}`)
	snapshot, err := makeProjectSnapshot(t.Context(), root, nil, "", first)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if records := snapshotRecords(t, snapshot); records[generatedCompose] != string(first) {
		t.Fatalf("missing generated config: %v", records)
	}
	second := []byte(`{"services":{"second":{}}}`)
	delta, err := makeProjectSnapshot(t.Context(), root, snapshot.Records, snapshot.Digest, second)
	if err != nil {
		t.Fatal(err)
	}
	defer delta.Close()
	records := snapshotRecords(t, delta)
	delete(records, deltaMetadata)
	if !reflect.DeepEqual(records, map[string]string{generatedCompose: string(second)}) {
		t.Fatalf("unexpected delta: %v", records)
	}
}

func writeFixture(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if output, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	return root
}

func snapshotRecords(t *testing.T, snapshot *Snapshot) map[string]string {
	t.Helper()
	content, err := gzip.NewReader(snapshot.File)
	if err != nil {
		t.Fatal(err)
	}
	defer content.Close()
	archive := tar.NewReader(content)
	records := map[string]string{}
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return records
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		records[header.Name] = string(body)
	}
}

func TestSnapshotIncludesWorktreeButExcludesSecrets(t *testing.T) {
	root := gitFixture(t)
	writeFixture(t, root, ".gitignore", "ignored\n")
	writeFixture(t, root, "ignored", "excluded")
	writeFixture(t, root, ".env.local", "excluded")
	writeFixture(t, root, "tls.key", "excluded")
	writeFixture(t, root, ".env.example", "sample")
	writeFixture(t, root, "app/raw.txt", "staged")
	if err := exec.Command("git", "-C", root, "add", "app/raw.txt", ".env.local").Run(); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "app/raw.txt", "uncommitted")
	before, err := os.ReadFile(filepath.Join(root, ".git/index"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := makeSnapshot(t.Context(), root, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	records := snapshotRecords(t, snapshot)
	want := map[string]string{".gitignore": "ignored\n", ".env.example": "sample", "app/raw.txt": "uncommitted"}
	if !reflect.DeepEqual(records, want) {
		t.Fatalf("snapshot files %v, want %v", records, want)
	}
	after, err := os.ReadFile(filepath.Join(root, ".git/index"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("snapshot modified the Git index")
	}
}

func TestDeltaContainsOnlyChangesAndDeletions(t *testing.T) {
	root := gitFixture(t)
	writeFixture(t, root, "public/message", "before")
	writeFixture(t, root, "delete-me", "obsolete")
	writeFixture(t, root, "unchanged", "unchanged")
	full, err := makeSnapshot(t.Context(), root, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	writeFixture(t, root, "public/message", "after")
	if err := os.Remove(filepath.Join(root, "delete-me")); err != nil {
		t.Fatal(err)
	}
	delta, err := makeSnapshot(t.Context(), root, full.Records, full.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer delta.Close()
	files := snapshotRecords(t, delta)
	var description Delta
	if err := json.Unmarshal([]byte(files[deltaMetadata]), &description); err != nil {
		t.Fatal(err)
	}
	delete(files, deltaMetadata)
	if description.Base != full.Digest || !reflect.DeepEqual(description.Deleted, []string{"delete-me"}) {
		t.Fatalf("incorrect delta metadata: %#v", description)
	}
	if !reflect.DeepEqual(files, map[string]string{"public/message": "after"}) {
		t.Fatalf("incorrect delta payload: %v", files)
	}
	noChange, err := makeSnapshot(t.Context(), root, delta.Records, delta.Digest)
	if err != nil || noChange != nil {
		t.Fatalf("unchanged source returned %v, %v", noChange, err)
	}
}

func TestSnapshotRejectsSymlinks(t *testing.T) {
	root := gitFixture(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "external")); err != nil {
		t.Fatal(err)
	}
	snapshot, err := makeSnapshot(t.Context(), root, nil, "")
	if err == nil {
		snapshot.Close()
		t.Fatal("uploaded a symbolic link")
	}
}

func TestSSHTargetValidation(t *testing.T) {
	for _, target := range []string{"devbox", "user@localhost", "lab@[2001:db8::1]", "2001:db8::1"} {
		if !validTarget(target) {
			t.Fatalf("rejected valid target %q", target)
		}
	}
	for _, target := range []string{"-oProxyCommand=false", "lab;test", "one two", "u@a@b", "user@"} {
		if validTarget(target) {
			t.Fatalf("accepted invalid target %q", target)
		}
	}
}

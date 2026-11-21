package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fixtureFile struct {
	name string
	body string
	mode int64
}

func fixtureArchive(t *testing.T, files ...fixtureFile) (string, []byte) {
	t.Helper()
	var output bytes.Buffer
	compressed := gzip.NewWriter(&output)
	archive := tar.NewWriter(compressed)
	for _, file := range files {
		if err := archive.WriteHeader(&tar.Header{Name: file.name, Mode: file.mode, Size: int64(len(file.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(file.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(output.Bytes())), output.Bytes()
}

func requireFile(t *testing.T, path, expected string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != expected {
		t.Fatalf("%s contains %q, want %q", path, body, expected)
	}
}

func TestDeltaPreservesBaseAndAtomicallyReplacesDirectory(t *testing.T) {
	workspace := t.TempDir()
	base, original := fixtureArchive(t,
		fixtureFile{"main", "unchanged", 0o755},
		fixtureFile{"static/a.txt", "old", 0o644},
	)
	if err := installSnapshot(workspace, base, bytes.NewReader(original)); err != nil {
		t.Fatal(err)
	}
	next, patch := fixtureArchive(t,
		fixtureFile{".remote-dev.delta.json", fmt.Sprintf(`{"base":%q,"deleted":["static/a.txt"]}`, base), 0o644},
		fixtureFile{"static", "replacement", 0o644},
	)
	for _, deployed := range []string{"", base, next} {
		accepted, err := installDelta(workspace, next, deployed, bytes.NewReader(patch))
		if err != nil || accepted != (deployed != "") {
			t.Fatalf("deployed %q: accepted %v, error %v", deployed, accepted, err)
		}
	}
	oldSource, err := verifySnapshot(workspace, base)
	if err != nil {
		t.Fatal(err)
	}
	newSource, err := verifySnapshot(workspace, next)
	if err != nil {
		t.Fatal(err)
	}
	requireFile(t, filepath.Join(oldSource, "static", "a.txt"), "old")
	requireFile(t, filepath.Join(newSource, "static"), "replacement")
	requireFile(t, filepath.Join(newSource, "main"), "unchanged")
	info, err := os.Stat(filepath.Join(newSource, "main"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("executable mode was lost: %v %v", info, err)
	}
}

func TestInvalidTransferDoesNotPublishSnapshot(t *testing.T) {
	workspace := t.TempDir()
	digest, payload := fixtureArchive(t, fixtureFile{"../outside", "bad", 0o644})
	if err := installSnapshot(workspace, digest, bytes.NewReader(payload)); err == nil || !strings.Contains(err.Error(), "unsafe workspace path") {
		t.Fatalf("expected unsafe path error, got %v", err)
	}
	if err := installSnapshot(workspace, digest, bytes.NewReader([]byte("different"))); err == nil || !strings.Contains(err.Error(), "digest check") {
		t.Fatalf("expected checksum error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "snapshots", digest)); !os.IsNotExist(err) {
		t.Fatalf("bad snapshot was published: %v", err)
	}
}

func TestRuntimeSourceCompletesInterruptedFileToDirectoryUpdate(t *testing.T) {
	workspace := t.TempDir()
	beforeDigest, beforeArchive := fixtureArchive(t, fixtureFile{"public", "old", 0o644}, fixtureFile{"keep", "safe", 0o644})
	afterDigest, afterArchive := fixtureArchive(t, fixtureFile{"public/index", "new", 0o644}, fixtureFile{"keep", "safe", 0o644})
	if err := installSnapshot(workspace, beforeDigest, bytes.NewReader(beforeArchive)); err != nil {
		t.Fatal(err)
	}
	if err := installSnapshot(workspace, afterDigest, bytes.NewReader(afterArchive)); err != nil {
		t.Fatal(err)
	}
	before, err := verifySnapshot(workspace, beforeDigest)
	if err != nil {
		t.Fatal(err)
	}
	after, err := verifySnapshot(workspace, afterDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := applyRuntimeSource(workspace, "", before); err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(workspace, "runtime-source")
	if err := os.Remove(filepath.Join(runtime, "public")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(runtime, "public"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtime, "public", "index"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := applyRuntimeSource(workspace, before, after); err != nil {
		t.Fatal(err)
	}
	requireFile(t, filepath.Join(runtime, "public", "index"), "new")
	if err := os.WriteFile(filepath.Join(runtime, "keep"), []byte("remote"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := applyRuntimeSource(workspace, before, after); err == nil {
		t.Fatal("runtime update accepted an unrelated remote edit")
	}
	requireFile(t, filepath.Join(runtime, "keep"), "remote")
}

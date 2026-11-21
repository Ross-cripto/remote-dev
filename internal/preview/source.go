package preview

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

const deltaMetadata = ".remote-dev.delta.json"

type FileStamp struct {
	Device   uint64
	Inode    uint64
	Size     int64
	Modified int64
	Changed  int64
	Mode     fs.FileMode
	Content  string
}

type Snapshot struct {
	File    *os.File
	Digest  string
	Bytes   int64
	Count   int
	Removed int
	Records map[string]FileStamp
}

type Delta struct {
	Base    string   `json:"base"`
	Deleted []string `json:"deleted"`
}

func (snapshot *Snapshot) Close() {
	defer os.Remove(snapshot.File.Name())
	snapshot.File.Close()
}

func git(ctx context.Context, root string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", root}, arguments...)...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("git: %s", strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("git: %w", err)
	}
	return output, nil
}

func worktreeRoot(ctx context.Context) (string, error) {
	current, err := os.Getwd()
	if err != nil {
		return "", err
	}
	output, err := git(ctx, current, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(strings.TrimSuffix(string(output), "\n"))
}

func privateSource(name string) bool {
	components := strings.Split(name, "/")
	for _, component := range components {
		switch component {
		case ".git", ".ssh", ".aws", ".gnupg", "node_modules", "__pycache__":
			return true
		}
	}
	basename := path.Base(name)
	switch basename {
	case "remote-dev.toml", ".npmrc", ".netrc", ".pypirc", "id_rsa", "id_ed25519":
		return true
	}
	switch path.Ext(basename) {
	case ".pem", ".key", ".p12", ".pfx":
		return true
	}
	if basename != ".env" && !strings.HasPrefix(basename, ".env.") {
		return false
	}
	switch path.Ext(basename) {
	case ".example", ".sample", ".template", ".dist":
		return false
	}
	return true
}

func stamp(info fs.FileInfo) FileStamp {
	stat := info.Sys().(*syscall.Stat_t)
	return FileStamp{
		Device: uint64(stat.Dev), Inode: stat.Ino, Size: info.Size(),
		Modified: info.ModTime().UnixNano(), Changed: changedAt(stat), Mode: info.Mode(),
	}
}

func checkSourceParents(root *os.Root, name string, checked map[string]bool) error {
	parent := path.Dir(name)
	if parent == "." || checked[parent] {
		return nil
	}
	if err := checkSourceParents(root, parent, checked); err != nil {
		return err
	}
	info, err := root.Lstat(parent)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("unsupported source directory: %s", parent)
	}
	checked[parent] = true
	return nil
}

func sourceRecords(ctx context.Context, root *os.Root) (map[string]FileStamp, error) {
	output, err := git(ctx, root.Name(), "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	records := map[string]FileStamp{}
	checked := map[string]bool{}
	for _, name := range strings.Split(string(output), "\x00") {
		if name == "" || privateSource(name) {
			continue
		}
		if !fs.ValidPath(name) || name == "." || name == deltaMetadata || name == ".remote-dev.generated.json" {
			return nil, fmt.Errorf("unsafe or reserved source path: %q", name)
		}
		if err := checkSourceParents(root, name, checked); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		info, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("unsupported source file: %s", name)
		}
		records[name] = stamp(info)
	}
	return records, nil
}

func diffSource(baseline, current map[string]FileStamp) (changed, deleted []string) {
	changed = []string{}
	deleted = []string{}
	for name, info := range current {
		if baseline[name] != info {
			changed = append(changed, name)
		}
	}
	for name := range baseline {
		if _, exists := current[name]; !exists {
			deleted = append(deleted, name)
		}
	}
	slices.Sort(changed)
	slices.Sort(deleted)
	return changed, deleted
}

func archiveRecord(archive *tar.Writer, root *os.Root, name string, expected FileStamp) error {
	source, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if stamp(info) != expected {
		return fmt.Errorf("source changed while capturing %s, retrying on the next scan", name)
	}
	mode := int64(0o644)
	if info.Mode()&0o100 != 0 {
		mode = 0o755
	}
	if err := archive.WriteHeader(&tar.Header{Name: name, Size: info.Size(), Mode: mode}); err != nil {
		return err
	}
	_, err = io.Copy(archive, source)
	return err
}

func archiveBytes(archive *tar.Writer, name string, content []byte, mode int64) error {
	if err := archive.WriteHeader(&tar.Header{Name: name, Size: int64(len(content)), Mode: mode}); err != nil {
		return err
	}
	_, err := archive.Write(content)
	return err
}

const generatedCompose = ".remote-dev.generated.json"

func writeArchive(output io.Writer, root *os.Root, changed []string, records map[string]FileStamp, delta *Delta, generated []byte) error {
	compressed := gzip.NewWriter(output)
	archive := tar.NewWriter(compressed)
	if delta != nil {
		metadata, err := json.Marshal(delta)
		if err != nil {
			return err
		}
		if err := archiveBytes(archive, deltaMetadata, metadata, 0o600); err != nil {
			return err
		}
	}
	for _, name := range changed {
		var err error
		if name == generatedCompose {
			err = archiveBytes(archive, name, generated, 0o644)
		} else {
			err = archiveRecord(archive, root, name, records[name])
		}
		if err != nil {
			return err
		}
	}
	return errors.Join(archive.Close(), compressed.Close())
}

func makeSnapshot(ctx context.Context, path string, baseline map[string]FileStamp, base string) (*Snapshot, error) {
	return makeProjectSnapshot(ctx, path, baseline, base, nil)
}

func addGenerated(records map[string]FileStamp, generated []byte) {
	if generated != nil {
		digest := sha256.Sum256(generated)
		records[generatedCompose] = FileStamp{Content: hex.EncodeToString(digest[:])}
	}
}

func makeProjectSnapshot(ctx context.Context, path string, baseline map[string]FileStamp, base string, generated []byte) (*Snapshot, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	records, err := sourceRecords(ctx, root)
	if err != nil {
		return nil, err
	}
	addGenerated(records, generated)
	changed, deleted := diffSource(baseline, records)
	var delta *Delta
	if base != "" {
		if len(changed)+len(deleted) == 0 {
			return nil, nil
		}
		delta = &Delta{Base: base, Deleted: deleted}
	}

	file, err := os.CreateTemp("", "remote-dev-go-*.tar.gz")
	if err != nil {
		return nil, err
	}
	snapshot := &Snapshot{File: file, Count: len(changed), Removed: len(deleted), Records: records}
	success := false
	defer func() {
		if !success {
			snapshot.Close()
		}
	}()

	hash := sha256.New()
	if err := writeArchive(io.MultiWriter(file, hash), root, changed, records, delta, generated); err != nil {
		return nil, err
	}
	latest, err := sourceRecords(ctx, root)
	if err != nil {
		return nil, err
	}
	addGenerated(latest, generated)
	if !maps.Equal(records, latest) {
		return nil, errors.New("source changed during snapshot capture, retrying on the next scan")
	}
	end, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	snapshot.Bytes = end
	snapshot.Digest = hex.EncodeToString(hash.Sum(nil))
	success = true
	return snapshot, nil
}

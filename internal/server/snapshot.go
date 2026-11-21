package server

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const deltaMetadata = ".remote-dev.delta.json"

var snapshotDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

type fileInventory map[string]string

type deltaManifest struct {
	Base    string   `json:"base"`
	Deleted []string `json:"deleted"`
}

func sourcePath(workspace, digest string) (string, error) {
	if !snapshotDigest.MatchString(digest) {
		return "", errors.New("invalid snapshot digest")
	}
	return filepath.Join(workspace, "snapshots", digest, "source"), nil
}

func workspacePath(root, name string) (string, error) {
	relative := path.Clean(name)
	if path.IsAbs(name) || relative == "." || !fs.ValidPath(relative) || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("unsafe workspace path: %q", name)
	}
	for _, component := range strings.Split(name, "/") {
		if component == ".." || component == ".git" {
			return "", fmt.Errorf("unsafe workspace path: %q", name)
		}
	}
	candidate := root
	for _, component := range strings.Split(relative, "/") {
		candidate = filepath.Join(candidate, component)
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("unexpected symlink in workspace path: %q", name)
		}
	}
	return candidate, nil
}

func fileDigest(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func inventory(root string) (fileInventory, error) {
	result := fileInventory{}
	err := filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("remote snapshot changed: unsupported entry %s", relative)
		}
		digest, err := fileDigest(filePath)
		if err != nil {
			return err
		}
		result[filepath.ToSlash(relative)] = digest
		return nil
	})
	return result, err
}

func verifySnapshot(workspace, digest string) (string, error) {
	source, err := sourcePath(workspace, digest)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(workspace, source)
	if err != nil {
		return "", err
	}
	if _, err := workspacePath(workspace, filepath.ToSlash(relative)); err != nil {
		return "", err
	}
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) || err == nil && !info.IsDir() {
		return "", errors.New("remote snapshot is incomplete. Run remote-dev up to retry")
	}
	if err != nil {
		return "", err
	}
	expected, err := readSnapshotInventory(source)
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("remote snapshot is incomplete. Run remote-dev up to retry")
	}
	if err != nil {
		return "", err
	}
	actual, err := inventory(source)
	if err != nil {
		return "", err
	}
	if !maps.Equal(expected, actual) {
		return "", errors.New("remote source changed. Preserve those changes before syncing this workspace")
	}
	return source, nil
}

func readSnapshotInventory(source string) (fileInventory, error) {
	filename, err := workspacePath(filepath.Dir(source), "files.json")
	if err != nil {
		return nil, err
	}
	var files fileInventory
	err = readJSON(filename, &files)
	return files, err
}

func readJSON(filename string, result any) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	return decodeJSON(file, result)
}

func decodeJSON(input io.Reader, result any) error {
	decoder := json.NewDecoder(input)
	decoder.UseNumber()
	if err := decoder.Decode(result); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("unexpected content after JSON document")
	}
	return nil
}

func writeJSON(filename string, content any) error {
	temporary, err := os.CreateTemp(filepath.Dir(filename), ".remote-dev-json-")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	encodeErr := json.NewEncoder(temporary).Encode(content)
	if err := errors.Join(encodeErr, temporary.Close()); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), filename)
}

func withVerifiedArchive(workspace, source, digest string, payload io.Reader, prepare func(*tar.Reader, string) (bool, error)) (bool, error) {
	snapshots := filepath.Dir(filepath.Dir(source))
	if _, err := workspacePath(workspace, "snapshots"); err != nil {
		return false, err
	}
	if err := os.Mkdir(snapshots, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return false, err
	}
	incoming, err := os.MkdirTemp(snapshots, "incoming-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(incoming)
	data, err := os.Create(filepath.Join(incoming, "source.tar"))
	if err != nil {
		return false, err
	}
	defer data.Close()
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(data, hash), payload); err != nil {
		return false, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return false, errors.New("snapshot transfer failed its digest check. Retry the operation")
	}
	if _, err := data.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	input := bufio.NewReader(data)
	var archiveData io.Reader = input
	header, err := input.Peek(2)
	if err != nil {
		return false, err
	}
	if header[0] == 0x1f && header[1] == 0x8b {
		compressed, err := gzip.NewReader(input)
		if err != nil {
			return false, err
		}
		defer compressed.Close()
		archiveData = compressed
	}
	staged := filepath.Join(incoming, "snapshot", "source")
	publish, err := prepare(tar.NewReader(archiveData), staged)
	if err != nil || !publish {
		return publish, err
	}
	if _, err := io.Copy(io.Discard, archiveData); err != nil {
		return false, err
	}
	return true, publishSnapshot(staged, source)
}

func publishSnapshot(staged, source string) error {
	files, err := inventory(staged)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(filepath.Dir(staged), "files.json"), files); err != nil {
		return err
	}
	return os.Rename(filepath.Dir(staged), filepath.Dir(source))
}

func fileExists(filename string) (bool, error) {
	_, err := os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func installSnapshot(workspace, digest string, payload io.Reader) error {
	source, err := sourcePath(workspace, digest)
	if err != nil {
		return err
	}
	exists, err := fileExists(filepath.Dir(source))
	if err != nil {
		return err
	}
	if exists {
		_, err := verifySnapshot(workspace, digest)
		return err
	}
	_, err = withVerifiedArchive(workspace, source, digest, payload, func(archive *tar.Reader, staged string) (bool, error) {
		if err := os.MkdirAll(staged, 0o755); err != nil {
			return false, err
		}
		if err := chmodDirectories(staged); err != nil {
			return false, err
		}
		return true, extractMembers(archive, staged, false)
	})
	return err
}

func extractMembers(archive *tar.Reader, root string, delta bool) error {
	seen := map[string]bool{}
	if delta {
		seen[deltaMetadata] = true
	}
	for {
		member, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if member.Typeflag != tar.TypeReg {
			return fmt.Errorf("unsupported snapshot member: %s", member.Name)
		}
		target, err := workspacePath(root, member.Name)
		if err != nil {
			return err
		}
		relative := path.Clean(member.Name)
		if seen[relative] {
			return fmt.Errorf("duplicate snapshot member: %s", member.Name)
		}
		seen[relative] = true
		if err := createSourceParents(root, target); err != nil {
			return err
		}
		if delta {
			if err := removeRegularFile(target); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		mode := fs.FileMode(0o644)
		if member.Mode&0o100 != 0 {
			mode = 0o755
		}
		if err := writeSourceFile(target, archive, mode); err != nil {
			return err
		}
	}
}

func writeSourceFile(target string, reader io.Reader, mode fs.FileMode) error {
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, reader)
	return errors.Join(copyErr, file.Chmod(mode), file.Close())
}

func chmodDirectories(directory string) error {
	return filepath.WalkDir(directory, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(filename, 0o755)
		}
		return nil
	})
}

func createSourceParents(root, target string) error {
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	for directory := parent; directory != root; directory = filepath.Dir(directory) {
		if err := os.Chmod(directory, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func readDeltaManifest(archive *tar.Reader) (deltaManifest, error) {
	header, err := archive.Next()
	if err != nil {
		return deltaManifest{}, fmt.Errorf("snapshot delta is missing its manifest: %w", err)
	}
	if header.Name != deltaMetadata || header.Typeflag != tar.TypeReg {
		return deltaManifest{}, errors.New("snapshot delta is missing its manifest")
	}
	var manifest deltaManifest
	if err := decodeJSON(archive, &manifest); err != nil {
		return deltaManifest{}, fmt.Errorf("invalid snapshot delta manifest: %w", err)
	}
	if !snapshotDigest.MatchString(manifest.Base) || manifest.Deleted == nil {
		return deltaManifest{}, errors.New("invalid snapshot delta manifest")
	}
	return manifest, nil
}

func installDelta(workspace, digest, deployed string, payload io.Reader) (bool, error) {
	source, err := sourcePath(workspace, digest)
	if err != nil {
		return false, err
	}
	reused := false
	published, err := withVerifiedArchive(workspace, source, digest, payload, func(archive *tar.Reader, staged string) (bool, error) {
		manifest, err := readDeltaManifest(archive)
		if err != nil {
			return false, err
		}
		exists, err := fileExists(filepath.Dir(source))
		if err != nil {
			return false, err
		}
		if digest == deployed || (manifest.Base == deployed && exists) {
			_, err := verifySnapshot(workspace, digest)
			reused = err == nil
			return false, err
		}
		if manifest.Base != deployed {
			return false, nil
		}
		base, err := verifySnapshot(workspace, manifest.Base)
		if err != nil {
			return false, err
		}
		if err := copySourceTree(base, staged); err != nil {
			return false, err
		}
		for _, name := range manifest.Deleted {
			if err := removeSnapshotFile(staged, name); err != nil {
				return false, err
			}
		}
		return true, extractMembers(archive, staged, true)
	})
	return published || reused, err
}

func copySourceTree(source, target string) error {
	return filepath.WalkDir(source, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, filename)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if entry.IsDir() {
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return err
			}
			return os.Chmod(destination, 0o755)
		}
		return copySourceFile(filename, destination)
	})
}

func copySourceFile(source, destination string) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported source file: %s", source)
	}
	return writeSourceFile(destination, file, info.Mode().Perm())
}

func removeRegularFile(filename string) error {
	info, err := os.Lstat(filename)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cannot remove non-file source entry: %s", filename)
	}
	return os.Remove(filename)
}

func removeSnapshotFile(root, name string) error {
	target, err := workspacePath(root, name)
	if err != nil {
		return err
	}
	if err := removeRegularFile(target); err != nil {
		return err
	}
	for directory := filepath.Dir(target); directory != root; directory = filepath.Dir(directory) {
		entries, err := os.ReadDir(directory)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			break
		}
		if err := os.Remove(directory); err != nil {
			return err
		}
	}
	return nil
}

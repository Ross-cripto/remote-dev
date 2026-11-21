package preview

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const stateDirectory = "remote-dev/go-preview-v1"

type Client struct {
	state string
	cache string
}

type transferStats struct {
	Bytes    int64  `json:"bytes"`
	Files    int    `json:"files"`
	Deleted  int    `json:"deleted"`
	Full     bool   `json:"full"`
	Finished string `json:"finished"`
}

func (transfer transferStats) String() string {
	kind := "delta"
	if transfer.Full {
		kind = "full snapshot"
	}
	return fmt.Sprintf("%s: %d bytes, %d files, %d deletions", kind, transfer.Bytes, transfer.Files, transfer.Deleted)
}

type Workspace struct {
	Root         string               `json:"root"`
	Key          string               `json:"key"`
	Connection   Connection           `json:"connection"`
	Compose      Compose              `json:"compose"`
	Mode         string               `json:"mode,omitempty"`
	HostOverride string               `json:"host_override,omitempty"`
	LegacyTunnel bool                 `json:"legacy_tunnel,omitempty"`
	ConfigDigest string               `json:"config_digest,omitempty"`
	FixedPorts   map[string]int       `json:"fixed_ports,omitempty"`
	Digest       string               `json:"digest,omitempty"`
	Records      map[string]FileStamp `json:"records,omitempty"`
	RemotePorts  map[string]int       `json:"remote_ports,omitempty"`
	LocalPorts   map[string]int       `json:"local_ports,omitempty"`
	Lease        string               `json:"lease,omitempty"`
	LastSync     *transferStats       `json:"last_sync,omitempty"`
	WatchError   string               `json:"watch_error,omitempty"`
}

func storagePath(variable, fallback string) (string, error) {
	root := os.Getenv(variable)
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, fallback)
	}
	return filepath.Abs(filepath.Join(root, stateDirectory))
}

func newClient() (Client, error) {
	state, err := storagePath("XDG_STATE_HOME", ".local/state")
	if err != nil {
		return Client{}, err
	}
	cache, err := storagePath("XDG_CACHE_HOME", ".cache")
	if err != nil {
		return Client{}, err
	}
	for _, directory := range []string{state, cache} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return Client{}, err
		}
	}
	return Client{state: state, cache: cache}, nil
}

func withLock(path string, operation func() error) error {
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	return operation()
}

func readJSON(path string, value any) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(content, value); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, value any) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := json.NewEncoder(file).Encode(value); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func randomID() (string, error) {
	var token [12]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func workspaceID(root string) string {
	digest := sha256.Sum256([]byte(root))
	return hex.EncodeToString(digest[:12])
}

func (client Client) workspacePath(root string) string {
	return filepath.Join(client.state, "workspace-"+workspaceID(root)+".json")
}

func (client Client) underWorkspaceLock(root string, operation func() error) error {
	return withLock(filepath.Join(client.state, "workspace-"+workspaceID(root)+".lock"), operation)
}

func (client Client) readWorkspace(root string) (Workspace, error) {
	var workspace Workspace
	err := readJSON(client.workspacePath(root), &workspace)
	if err != nil {
		return workspace, err
	}
	if workspace.Root != root || len(workspace.Key) != 24 {
		return Workspace{}, fmt.Errorf("invalid workspace record for %s", root)
	}
	if _, err := hex.DecodeString(workspace.Key); err != nil {
		return Workspace{}, fmt.Errorf("invalid workspace ID for %s", root)
	}
	return workspace, nil
}

func (client Client) saveWorkspace(workspace Workspace) error {
	return writeJSON(client.workspacePath(workspace.Root), workspace)
}

func (client Client) connectionPath() string {
	return filepath.Join(client.state, "connection.json")
}

func (client Client) socket(workspace Workspace) string {
	cache := client.cache
	if workspace.LegacyTunnel {
		cache = client.legacyCache()
	}
	return filepath.Join(cache, workspace.Key[:16]+".sock")
}

func finishTransfer(snapshot *Snapshot, full bool) transferStats {
	return transferStats{
		Bytes: snapshot.Bytes, Files: snapshot.Count, Deleted: snapshot.Removed,
		Full: full, Finished: time.Now().Format(time.RFC3339),
	}
}

package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/unreallabsai/unreal-agent/harness/session"
)

// ErrBusy means another process holds the session.
var ErrBusy = errors.New("session busy")

// Layout names the files under the state directory.
type Layout struct{ Root string }

// SessionsDir holds <id>.session.jsonl (the localfile store).
func (l Layout) SessionsDir() string { return filepath.Join(l.Root, "sessions") }

// MetaDir holds <id>.json sidecars.
func (l Layout) MetaDir() string { return filepath.Join(l.Root, "meta") }

// LocksDir holds <id>.lock files used only as flock targets.
func (l Layout) LocksDir() string { return filepath.Join(l.Root, "locks") }

// OperationsDir holds tool output files for one session.
func (l Layout) OperationsDir(id session.ID) string {
	return filepath.Join(l.Root, "operations", string(id))
}

// SessionFile is the localfile store's session log.
func (l Layout) SessionFile(id session.ID) string {
	return filepath.Join(l.SessionsDir(), string(id)+".session.jsonl")
}

// Exists reports whether the session log exists.
func (l Layout) Exists(id session.ID) bool {
	_, err := os.Stat(l.SessionFile(id))
	return err == nil
}

// Meta is the per-session sidecar: things the session file does not record
// (the model id never reaches it, llm/model.go:118-124).
type Meta struct {
	Cwd       string `json:"cwd"`
	Model     string `json:"model"`
	Effort    string `json:"effort,omitempty"`
	CreatedAt string `json:"created_at"`
}

// ReadMeta loads the sidecar.
func (l Layout) ReadMeta(id session.ID) (Meta, error) {
	var meta Meta
	data, err := os.ReadFile(filepath.Join(l.MetaDir(), string(id)+".json"))
	if err != nil {
		return meta, err
	}
	return meta, json.Unmarshal(data, &meta)
}

// WriteMeta atomically replaces the sidecar (temp file + rename).
func (l Layout) WriteMeta(id session.ID, meta Meta) error {
	if err := os.MkdirAll(l.MetaDir(), 0o700); err != nil {
		return fmt.Errorf("create meta dir: %w", err)
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(l.MetaDir(), ".meta-*")
	if err != nil {
		return fmt.Errorf("write meta: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("write meta: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write meta: %w", err)
	}
	return os.Rename(tmp.Name(), filepath.Join(l.MetaDir(), string(id)+".json"))
}

// Lock is an exclusive kernel flock on a session. The kernel releases it
// when the process dies, so it can never go stale.
type Lock struct{ file *os.File }

// AcquireLock takes LOCK_EX|LOCK_NB on <locks>/<id>.lock. It locks a sidecar
// rather than <id>.session.jsonl because localfile.Store.Create publishes
// via rename over the target (localfile/store.go:386-408), which would swap
// the inode under a lock held on the session log.
func (l Layout) AcquireLock(id session.ID) (*Lock, error) {
	if err := os.MkdirAll(l.LocksDir(), 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(l.LocksDir(), string(id)+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open session lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s is open in another process", ErrBusy, id)
		}
		return nil, fmt.Errorf("lock session %s: %w", id, err)
	}
	return &Lock{file: f}, nil
}

// Release drops the lock. Safe to call more than once.
func (lk *Lock) Release() {
	if lk == nil || lk.file == nil {
		return
	}
	_ = syscall.Flock(int(lk.file.Fd()), syscall.LOCK_UN)
	lk.file.Close()
	lk.file = nil
}

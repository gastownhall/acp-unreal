package acpagent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/session"
)

// BoundMode selects how the first session/new of the process picks its id.
type BoundMode int

// Bound modes, in priority order.
const (
	Unbound BoundMode = iota
	// BoundGC is a gc launch: open-or-create an id derived from
	// GC_SESSION_ID + GC_CONTINUATION_EPOCH, without replay. gc restarts
	// resume the conversation; `gc session reset` bumps the epoch.
	BoundGC
	// BoundCreate is --session-id: create K; error if it exists.
	BoundCreate
	// BoundResume is --resume: open K without replay; K must exist.
	BoundResume
)

// Bound is the resolved bound-mode target.
type Bound struct {
	Mode BoundMode
	ID   session.ID
}

// ParentPIDEnv is exported to every tool of an acp-unreal process (value:
// its pid). A process that sees it was started by another agent's tool, so
// its GC_SESSION_ID is inherited ambient identity, not its own.
const ParentPIDEnv = "ACP_UNREAL_PARENT_PID"

// ResolveBound applies the priority GC_SESSION_ID > --session-id > --resume.
// GC_SESSION_ID is ignored under a parent acp-unreal (ParentPIDEnv set).
func ResolveBound(getenv func(string) string, sessionIDFlag, resumeFlag string) (Bound, error) {
	if raw := getenv("GC_SESSION_ID"); raw != "" && getenv(ParentPIDEnv) == "" {
		return Bound{Mode: BoundGC, ID: GCSessionID(raw, getenv("GC_CONTINUATION_EPOCH"))}, nil
	}
	if sessionIDFlag != "" {
		if !ValidSessionID(sessionIDFlag) {
			return Bound{}, errors.New("--session-id must contain only ASCII letters, digits and dashes")
		}
		return Bound{Mode: BoundCreate, ID: session.ID(sessionIDFlag)}, nil
	}
	if resumeFlag != "" {
		if !ValidSessionID(resumeFlag) {
			return Bound{}, errors.New("--resume must contain only ASCII letters, digits and dashes")
		}
		return Bound{Mode: BoundResume, ID: session.ID(resumeFlag)}, nil
	}
	return Bound{}, nil
}

// GCSessionID derives the Unreal session id for a gc session incarnation:
// "gc-" + Sanitize(gcSessionID) + "-e" + digits(epoch, default 1). This
// follows the zcode adapter precedent in gc
// (internal/worker/adapters/zcode/zcode-repl:157-203).
func GCSessionID(gcSessionID, epoch string) session.ID {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, epoch)
	if digits == "" {
		digits = "1"
	}
	return session.ID("gc-" + Sanitize(gcSessionID) + "-e" + digits)
}

// Sanitize maps characters outside [A-Za-z0-9-] to '-'; if anything was
// mapped it appends "-" + the first 8 hex digits of sha256(raw), so distinct
// raw ids stay distinct. Unreal session ids allow only letters, digits and
// dashes (localfile/store.go:470-485).
func Sanitize(raw string) string {
	mapped := false
	out := strings.Map(func(r rune) rune {
		if validIDRune(r) {
			return r
		}
		mapped = true
		return '-'
	}, raw)
	if !mapped {
		return out
	}
	sum := sha256.Sum256([]byte(raw))
	return out + "-" + hex.EncodeToString(sum[:])[:8]
}

func validIDRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
}

// ValidSessionID mirrors the localfile store's id rule; it also keeps ids
// safe as file names.
func ValidSessionID(id string) bool {
	if id == "" || len(id) > 200 {
		return false
	}
	for _, r := range id {
		if !validIDRune(r) {
			return false
		}
	}
	return true
}

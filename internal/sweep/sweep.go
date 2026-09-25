// Package sweep stops tool descendants that escaped their tool's process
// group (setsid daemons, `tmux new -d`, double forks) when the agent shuts
// down, without touching processes a tool deliberately handed off.
//
// Ownership is an environment marker, the same contract gc's session orphan
// sweep uses: every process this agent starts inherits ACP_UNREAL_OWNERS
// (a colon-separated list that includes this agent's random token), and,
// under gc, the session's GC_SESSION_ID. A process is this agent's leftover
// only if its environment still carries both. gc strips GC_SESSION_ID from
// the city infrastructure it detaches on purpose (a drift-respawned
// supervisor, managed Dolt and its scope watchdog), so those are spared.
// A process that clears its environment (env -i) is not found.
package sweep

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// OwnersEnv holds the colon-separated tokens of every acp-unreal ancestor of
// a process.
const OwnersEnv = "ACP_UNREAL_OWNERS"

// gcSessionEnv is gc's per-session identity variable.
const gcSessionEnv = "GC_SESSION_ID"

// Marker identifies this agent's descendants.
type Marker struct {
	token     string
	gcSession string
}

// Mark creates this agent's token and appends it to OwnersEnv in the process
// environment, which every tool inherits. It also captures GC_SESSION_ID:
// when set, a descendant must carry the same value to be swept.
func Mark() (Marker, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Marker{}, fmt.Errorf("sweep token: %w", err)
	}
	m := Marker{token: hex.EncodeToString(raw[:]), gcSession: os.Getenv(gcSessionEnv)}
	owners := m.token
	if prev := os.Getenv(OwnersEnv); prev != "" {
		owners = prev + ":" + m.token
	}
	if err := os.Setenv(OwnersEnv, owners); err != nil {
		return Marker{}, fmt.Errorf("export %s: %w", OwnersEnv, err)
	}
	return m, nil
}

// Matches reports whether a NUL-separated environment block (the format of
// /proc/<pid>/environ) marks a descendant of this agent.
func (m Marker) Matches(environ []byte) bool {
	if m.token == "" {
		return false
	}
	owned, session := false, m.gcSession == ""
	for entry := range bytes.SplitSeq(environ, []byte{0}) {
		name, value, ok := strings.Cut(string(entry), "=")
		if !ok {
			continue
		}
		switch name {
		case OwnersEnv:
			for token := range strings.SplitSeq(value, ":") {
				if token == m.token {
					owned = true
				}
			}
		case gcSessionEnv:
			if m.gcSession != "" && value == m.gcSession {
				session = true
			}
		}
	}
	return owned && session
}

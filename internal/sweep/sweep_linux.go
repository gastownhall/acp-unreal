package sweep

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Supported reports whether this platform can find escaped descendants.
const Supported = true

// pollInterval is how often Stop re-checks the signalled processes.
const pollInterval = 20 * time.Millisecond

// Stop sends SIGTERM to every live process whose environment carries this
// agent's marker, waits up to grace for them to exit, then SIGKILLs the
// ones still running and still marked. It signals single processes, never
// process groups, and waits for nothing it does not own (the agent is not
// their parent). Call it after the tool manager drained.
func (m Marker) Stop(log *slog.Logger, grace time.Duration) {
	found := m.find()
	if len(found) == 0 {
		return
	}
	for _, pid := range found {
		if err := syscall.Kill(pid, syscall.SIGTERM); err == nil {
			log.Info("stopping leftover descendant", "pid", pid, "signal", "SIGTERM")
		}
	}
	deadline := time.Now().Add(grace)
	for {
		remaining := m.filter(found)
		if len(remaining) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, pid := range remaining {
				if err := syscall.Kill(pid, syscall.SIGKILL); err == nil {
					log.Info("killed leftover descendant", "pid", pid, "signal", "SIGKILL")
				}
			}
			return
		}
		time.Sleep(pollInterval)
	}
}

// find lists every live marked process except this one.
func (m Marker) find() []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			pids = append(pids, pid)
		}
	}
	return m.filter(pids)
}

// filter keeps the pids that are live (not zombies), not this process, and
// marked. A zombie's environ reads empty, so it never matches anyway.
func (m Marker) filter(pids []int) []int {
	self := os.Getpid()
	var out []int
	for _, pid := range pids {
		if pid == self || !live(pid) {
			continue
		}
		env, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
		if err != nil || !m.Matches(env) {
			continue
		}
		out = append(out, pid)
	}
	return out
}

func live(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	s := string(data)
	end := strings.LastIndexByte(s, ')')
	if end < 0 || end+2 >= len(s) {
		return false
	}
	state := s[end+2]
	return state != 'Z' && state != 'X'
}

// Package reaper makes the process a child subreaper so tool descendants
// that detach with setsid (daemons, `tmux new -d`) are re-parented to it
// instead of init, reaps them as they exit, and kills them at shutdown.
//
// Limits: only descendants that are re-parented (their parent died) or are
// still reachable through a child's session or process group are found; a
// descendant that double-forks into a session whose members are all
// grandchildren of a live non-tool process is not. Linux only.
package reaper

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const prSetChildSubreaper = 36

// Enable marks the process as a child subreaper (prctl
// PR_SET_CHILD_SUBREAPER).
func Enable() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_CHILD_SUBREAPER): %w", errno)
	}
	return nil
}

// Supported reports whether this platform has a subreaper.
const Supported = true

type proc struct {
	pid, ppid, pgid, sid int
	state                string
}

func readProc(pid int) (proc, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return proc{}, false
	}
	s := string(data)
	end := strings.LastIndexByte(s, ')')
	if end < 0 || end+2 > len(s) {
		return proc{}, false
	}
	fields := strings.Fields(s[end+2:])
	if len(fields) < 4 {
		return proc{}, false
	}
	p := proc{pid: pid, state: fields[0]}
	p.ppid, _ = strconv.Atoi(fields[1])
	p.pgid, _ = strconv.Atoi(fields[2])
	p.sid, _ = strconv.Atoi(fields[3])
	return p, true
}

func ownSession() int {
	self, _ := readProc(os.Getpid())
	return self.sid
}

// children lists this process's children via /proc/self/task/*/children.
func children() []proc {
	tasks, _ := filepath.Glob("/proc/self/task/*/children")
	var out []proc
	for _, task := range tasks {
		data, err := os.ReadFile(task)
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(string(data)) {
			if pid, err := strconv.Atoi(field); err == nil {
				if p, ok := readProc(pid); ok {
					out = append(out, p)
				}
			}
		}
	}
	return out
}

func allProcs() []proc {
	entries, _ := os.ReadDir("/proc")
	var out []proc
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			if p, ok := readProc(pid); ok {
				out = append(out, p)
			}
		}
	}
	return out
}

// reapAdopted waits for exited children outside this process's session.
// Tool leaders started by the library share this session (Setpgid only,
// primitives/process.go:609) and are waited by os/exec, so they are never
// touched; only adopted setsid escapees are reaped.
func reapAdopted(own int) {
	for _, child := range children() {
		if child.state == "Z" && child.sid != own {
			var status syscall.WaitStatus
			_, _ = syscall.Wait4(child.pid, &status, syscall.WNOHANG, nil)
		}
	}
}

// Run reaps adopted children on every SIGCHLD until ctx ends.
func Run(ctx context.Context) {
	own := ownSession()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGCHLD)
	defer signal.Stop(signals)
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			reapAdopted(own)
		}
	}
}

// KillDescendants SIGKILLs what is left once every session has drained:
// each remaining child, its process group, and every process in the
// session of a child that escaped into its own session; then it reaps the
// children. Call it only after the tool manager drained, since it also
// kills (and reaps) library-owned children.
func KillDescendants(log *slog.Logger) {
	self := os.Getpid()
	own := ownSession()
	ownGroup := syscall.Getpgrp()
	kids := children()
	sessions := map[int]bool{}
	for _, child := range kids {
		if child.state != "Z" {
			if child.pgid > 1 && child.pgid != ownGroup {
				_ = syscall.Kill(-child.pgid, syscall.SIGKILL)
			}
			_ = syscall.Kill(child.pid, syscall.SIGKILL)
			log.Info("killed leftover descendant", "pid", child.pid, "pgid", child.pgid, "sid", child.sid)
		}
		if child.sid != own && child.sid > 1 {
			sessions[child.sid] = true
		}
	}
	if len(sessions) > 0 {
		for _, p := range allProcs() {
			if sessions[p.sid] && p.pid != self && p.state != "Z" {
				_ = syscall.Kill(p.pid, syscall.SIGKILL)
			}
		}
	}
	for _, child := range kids {
		var status syscall.WaitStatus
		_, _ = syscall.Wait4(child.pid, &status, 0, nil)
	}
}

package main

import (
	"os"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// The re-exec'd process inherits the key pipe only if close-on-exec is clear.
func TestKeyPipeIsInheritedAndCarriesTheKey(t *testing.T) {
	fd, err := keyPipe("dummy-key")
	if err != nil {
		t.Fatal(err)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC != 0 {
		t.Fatal("key pipe fd is close-on-exec; the re-exec would lose it")
	}
	key, err := readKeyFD(strconv.Itoa(fd))
	if err != nil || key != "dummy-key" {
		t.Fatalf("readKeyFD = %q, %v", key, err)
	}
}

// Without /proc (macOS, BSDs) the re-exec uses os.Executable.
func TestReexecPathFallsBackWithoutProc(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got, err := reexecPath("/nonexistent/proc/self/exe")
	if err != nil || got != self {
		t.Fatalf("reexecPath = %q, %v; want %q", got, err, self)
	}
	if _, err := os.Stat(procSelfExe); err == nil {
		if got, _ := reexecPath(procSelfExe); got != procSelfExe {
			t.Fatalf("reexecPath = %q, want %s where it exists", got, procSelfExe)
		}
	}
}

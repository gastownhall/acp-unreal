package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/gastownhall/acp-unreal/internal/credenv"
)

// keyFDEnv carries the inherited pipe fd holding the API key across the
// credential re-exec ("none" when there is no key). Its presence is also
// the loop guard: a process that sees it never re-execs again.
const keyFDEnv = "ACP_UNREAL_KEY_FD"

// maxKeyBytes bounds what is read from a key file or the key pipe.
const maxKeyBytes = 16 << 10

// loadAPIKey returns the provider key and makes sure the process environment
// the kernel shows in /proc/<pid>/environ holds no credential.
//
// os.Unsetenv edits only Go's copy of the environment; /proc/<pid>/environ
// keeps the exec-time block, and every tool can read its parent's. So when
// the environment holds anything the policy scrubs, the process re-execs
// itself (same pid, same argv) with the scrubbed environment and passes the
// key over an inherited pipe. It returns only in the final process.
func loadAPIKey(o options) (string, error) {
	policy := credenv.NewPolicy(o.apiKeyEnv, splitCSV(o.scrubEnv), splitCSV(o.keepEnv))
	if fd, ok := os.LookupEnv(keyFDEnv); ok {
		_ = os.Unsetenv(keyFDEnv)
		// Already re-exec'd. Anything still matching (only possible if the
		// guard variable came from outside) is at least kept from tools.
		_, removed := policy.Scrub(os.Environ())
		for _, name := range removed {
			_ = os.Unsetenv(name)
		}
		return readKeyFD(fd)
	}
	var key string
	if o.apiKeyFile != "" {
		k, err := readKeyFile(o.apiKeyFile)
		if err != nil {
			return "", err
		}
		key = k
	} else {
		key = os.Getenv(o.apiKeyEnv)
	}
	kept, removed := policy.Scrub(os.Environ())
	if len(removed) == 0 {
		return key, nil
	}
	fdValue := "none"
	if key != "" {
		fd, err := keyPipe(key)
		if err != nil {
			return "", err
		}
		fdValue = strconv.Itoa(fd)
	}
	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}
	err = syscall.Exec("/proc/self/exe", os.Args, append(kept, keyFDEnv+"="+fdValue))
	return "", fmt.Errorf("re-exec %s with a scrubbed environment: %w", self, err)
}

// keyPipe writes key into a new pipe and returns its read end with
// close-on-exec cleared, so the re-exec'd process inherits it.
func keyPipe(key string) (int, error) {
	if len(key) > maxKeyBytes {
		return 0, errors.New("API key is too large")
	}
	var fds [2]int
	if err := syscall.Pipe2(fds[:], syscall.O_CLOEXEC); err != nil {
		return 0, fmt.Errorf("key pipe: %w", err)
	}
	// A pipe buffer (>= 4 KiB, 64 KiB on Linux) holds the key, so this
	// write does not block without a reader.
	if _, err := syscall.Write(fds[1], []byte(key)); err != nil {
		syscall.Close(fds[0])
		syscall.Close(fds[1])
		return 0, fmt.Errorf("key pipe: %w", err)
	}
	syscall.Close(fds[1])
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fds[0]), syscall.F_SETFD, 0); errno != 0 {
		syscall.Close(fds[0])
		return 0, fmt.Errorf("key pipe: %w", errno)
	}
	return fds[0], nil
}

func readKeyFD(value string) (string, error) {
	if value == "none" {
		return "", nil
	}
	fd, err := strconv.Atoi(value)
	if err != nil || fd < 3 {
		return "", fmt.Errorf("%s=%q is not an inherited fd", keyFDEnv, value)
	}
	syscall.CloseOnExec(fd)
	f := os.NewFile(uintptr(fd), "api-key")
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxKeyBytes+1))
	if err != nil {
		return "", fmt.Errorf("read key from fd %d: %w", fd, err)
	}
	return string(data), nil
}

// readKeyFile reads a key file that only its owner may read or write.
func readKeyFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("--api-key-file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("--api-key-file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("--api-key-file %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("--api-key-file %s has mode %04o; it must not be accessible by group or others (chmod 600)", path, perm)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyBytes+1))
	if err != nil {
		return "", fmt.Errorf("--api-key-file: %w", err)
	}
	if len(data) > maxKeyBytes {
		return "", fmt.Errorf("--api-key-file %s is too large", path)
	}
	return strings.TrimSpace(string(data)), nil
}

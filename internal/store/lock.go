package store

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ErrLockHeld means another process is currently the writer.
var ErrLockHeld = errors.New("writer lock held by another process")

// FileLock holds an exclusive file lock for the lifetime of the process.
// On Close (or process death) the kernel releases the lock.
type FileLock struct {
	path string
	f    *os.File
}

// TryLock attempts to acquire an exclusive, non-blocking flock on path.
// Returns ErrLockHeld if another process owns the lock. Successfully
// returned locks are released by FileLock.Close or process exit.
func TryLock(path string) (*FileLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLockHeld
		}
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	// Stamp the lock file with the holder's PID — purely informational
	// for debugging. Truncate first so a stale PID from a dead writer
	// isn't confusing.
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return &FileLock{path: path, f: f}, nil
}

// Close releases the lock and removes the lock file. The file removal
// is best-effort — the actual lock is the fcntl record on the open
// fd, so closing it is what frees the lock.
func (l *FileLock) Close() error {
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	return l.f.Close()
}

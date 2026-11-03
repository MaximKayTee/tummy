// Package lock gives one process at a time a named lock, across processes.
package lock

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

var ErrBusy = errors.New("already running")

type Lock struct{ f *os.File }

// Acquire takes dir/name.lock without blocking. ErrBusy if another holder has it, in this
// process or another. The directory is created 0700, the file 0600.
func Acquire(dir, name string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Free reports whether dir/name.lock could be taken right now.
func Free(dir, name string) bool {
	l, err := Acquire(dir, name)
	if err != nil {
		return false
	}
	l.Release()
	return true
}

func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
	l.f = nil
	return err
}

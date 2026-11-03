package engine

import "tummy/internal/lock"

// acquireFor holds a replication lock for tests.
func acquireFor(dir, name string) (func(), error) {
	l, err := lock.Acquire(dir, name)
	if err != nil {
		return nil, err
	}
	return func() { l.Release() }, nil
}

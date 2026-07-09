//go:build !unix

package gc

import "errors"

var errLockHeld = errors.New("lock already held")

func tryLockExclusive(string) (func(), error) {
	return func() {}, nil
}

package platform

import "errors"

// ErrLockHeld is returned by non-blocking lock acquisition when the lock is busy.
var ErrLockHeld = errors.New("lock already held")

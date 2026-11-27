//go:build unix

package cli

import "syscall"

// openNoBlock makes opening a FIFO that replaced a checked file return at once instead of
// blocking forever; it is harmless for regular files. openNoFollow refuses symlink targets.
const (
	openNoBlock  = syscall.O_NONBLOCK
	openNoFollow = syscall.O_NOFOLLOW
)

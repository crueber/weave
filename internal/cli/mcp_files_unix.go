//go:build unix

package cli

import "syscall"

// openNoBlock makes opening a FIFO that replaced a checked file return at once instead of
// blocking forever; it is harmless for regular files.
const openNoBlock = syscall.O_NONBLOCK

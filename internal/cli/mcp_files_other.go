//go:build !unix

package cli

// openNoBlock is a no-op where O_NONBLOCK does not exist (Windows has no FIFOs in the namespace
// os.Root can reach; device paths are refused by windowsPathProblem).
const openNoBlock = 0

//go:build !unix

package cli

// openNoBlock and openNoFollow are no-ops where POSIX open flags do not exist (Windows has no
// FIFOs in the namespace os.Root can reach; device paths are refused by windowsPathProblem).
const (
	openNoBlock  = 0
	openNoFollow = 0
)

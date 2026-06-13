//go:build windows

package tunnel

import "os"

// Windows has no flock; the O_CREATE lock file plus the manager's in-process
// bookkeeping is the only guard there. Windows callers use --tcp-tunnels, so
// the unix-socket fight this lock prevents does not arise.
func flockExclusiveNB(*os.File) error { return nil }

func flockRelease(*os.File) {}

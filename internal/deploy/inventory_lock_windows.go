//go:build windows

package deploy

import "os"

// flockExclusive is a no-op on Windows: the atomic tmp+rename write keeps the
// inventory file itself consistent; concurrent read-modify-write cycles are
// last-writer-wins there.
func flockExclusive(*os.File) error { return nil }

func flockRelease(*os.File) {}

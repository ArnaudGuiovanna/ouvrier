//go:build js || plan9

package sandbox

import (
	"fmt"
	"os"
)

type sandboxWriteHook func()

func (s *Sandbox) writeFileAtomic(path string, _ []byte, _ os.FileMode, _ sandboxWriteHook) error {
	return fmt.Errorf("%w: atomic anchored file sinks are unsupported on this platform", ErrInvalidWorkspace)
}

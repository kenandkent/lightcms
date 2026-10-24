//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package artifacts

import (
	"fmt"
	"os"
)

func tryExclusive(*os.File) error {
	return fmt.Errorf("artifact lifecycle locking unavailable on this platform")
}
func unlockFile(*os.File) error { return nil }

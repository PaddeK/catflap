//go:build !windows

package setup

import (
	"fmt"
	"io"
	"os"
)

// removeBinary deletes the catflap program (a running program may delete
// its own file on Unix).
func removeBinary(exe string, out io.Writer) error {
	if err := os.Remove(exe); err != nil {
		return err
	}
	fmt.Fprintf(out, "    deleted %s\n", exe)
	return nil
}

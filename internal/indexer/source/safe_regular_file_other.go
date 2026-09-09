//go:build !unix

package source

import (
	"fmt"
	"os"
)

const regularFilesystemReadSupported = false

func openRootRegularFile(_ *os.Root, name string) (*os.File, error) {
	return nil, fmt.Errorf("%s: %w", name, ErrRegularReadUnsupported)
}

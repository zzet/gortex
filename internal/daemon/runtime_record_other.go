//go:build !darwin && !linux && !windows

package daemon

import (
	"errors"
	"os"
)

func openExistingRuntimeRecord(_ string) (*os.File, error) {
	return nil, errors.New("owned runtime no-follow record reader unsupported on this platform")
}

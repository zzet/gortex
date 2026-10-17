//go:build windows

package goanalysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGoAnalysis_RelativePathWindowsCompilerPositions(t *testing.T) {
	for _, tt := range []struct {
		name, filename, root, want string
	}{
		{"exported forward slashes", "C:/checkout/impl/impl.go", `C:\checkout`, "impl/impl.go"},
		{"native filename", `C:\checkout\impl\impl.go`, "C:/checkout", "impl/impl.go"},
		{"case equivalent root", "c:/CHECKOUT/impl/impl.go", `C:\checkout`, "impl/impl.go"},
		{"sibling prefix", "C:/checkout-other/impl.go", `C:\checkout`, ""},
		{"different drive", "D:/checkout/impl.go", `C:\checkout`, ""},
		{"parent traversal", "C:/checkout/../outside/impl.go", `C:\checkout`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, relativePath(tt.filename, tt.root))
		})
	}
}

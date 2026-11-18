package mcp

import (
	"bytes"
	"strings"
	"testing"

	wire "github.com/gortexhq/gcx-go"
	"github.com/stretchr/testify/require"
)

func TestSearchGoldenNormalizationUsesActualGCXPathEscaping(t *testing.T) {
	for _, root := range []string{`D:\owned\checkout`, `\\host\share\checkout`, "/owned/tab\tand\nnewline"} {
		t.Run(root, func(t *testing.T) {
			encode := func(path string) string {
				var buffer bytes.Buffer
				encoder := wire.NewEncoder(&buffer, wire.Header{Tool: "search_symbols", Fields: []string{"id", "path_abs", "sig", "unrelated", "empty"}})
				require.NoError(t, encoder.WriteComment("preserve header, comment and trailing empty cell"))
				require.NoError(t, encoder.WriteRow("server.go::Handle", path, "func literal(\"D:\\code\")\tline\nnext", root+"-sibling", ""))
				require.NoError(t, encoder.WriteRow("unrelated.go::Other", root+"-sibling/unrelated.go", `func keep("D:\literal")`, "", ""))
				require.NoError(t, encoder.Close())
				return buffer.String()
			}
			separator := "/"
			if strings.Contains(root, `\`) {
				separator = `\`
			}
			actual := encode(root + separator + "server" + separator + "server.go")
			want := encode("$ROOT/server/server.go")
			require.Equal(t, want, normalizeSearchGoldenRoots(actual, []string{root}), "decode only the path cell; unrelated wire bytes must stay exact")
		})
	}
}

package mcp

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
)

func TestPreparedProjectionSyntaxGateParsesCleanCandidateOnce(t *testing.T) {
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	ctx, release, err := acquireMutationPath(context.Background(), path)
	require.NoError(t, err)
	defer release()
	before := []byte("package p\nfunc Good() {}\n")
	for _, test := range []struct {
		name, source string
		parses       uint64
		blocked      bool
	}{
		{"clean", "package p\nfunc Better() {}\n", 1, false},
		{"regression", "package p\nfunc Better() { _ = }\n", 2, true},
		{"repair", "package p\nfunc Better() {}\n", 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			old := before
			if test.name == "repair" {
				old = []byte("package p\nfunc Good() { _ = }\n")
			}
			count := parser.ParseCount()
			entry, verdict := s.prepareAcknowledgedProjection(ctx, "main.go", path, old, []byte(test.source))
			parses := parser.ParseCount() - count
			require.NotNil(t, entry)
			require.True(t, verdict.Checked)
			require.Equal(t, test.blocked, verdict.Blocked)
			require.Equal(t, test.parses, parses, "projection and gate must share the exact candidate parse; only broken candidates parse old bytes")
		})
	}
}

func TestProjectedParseGateUnknownOrDifferentBytesUseOriginalGate(t *testing.T) {
	before := []byte("package p\nfunc Good() {}\n")
	after := []byte("package p\nfunc Bad() { _ = }\n")
	sum := blake3.Sum256(after)
	hash := hex.EncodeToString(sum[:])
	for _, test := range []struct {
		name       string
		projection *indexer.Projection
	}{
		{"absent", nil},
		{"old or plugin response", &indexer.Projection{Language: "go", ContentHash: hash}},
		{"invalid negative", &indexer.Projection{Language: "go", ContentHash: hash, ParseChecked: true, ParseErrors: -1}},
		{"different parsed bytes", &indexer.Projection{Language: "go", ContentHash: "different parsed source", ParseChecked: true}},
		{"different grammar", &indexer.Projection{Language: "python", ContentHash: hash, ParseChecked: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			expected := checkParseGate("main.go", before, after)
			count := parser.ParseCount()
			actual := projectedParseGate("main.go", before, after, test.projection)
			require.Equal(t, expected, actual)
			require.True(t, actual.Blocked)
			require.EqualValues(t, 2, parser.ParseCount()-count, "unknown proof must parse candidate and old bytes")
		})
	}
}

func TestProjectedParseGateKeepsBinaryAndOldErrorSemantics(t *testing.T) {
	for _, test := range []struct{ before, after string }{
		{"package p\n", "package p\nfunc F() { _ = }\n"},
		{"package p\nfunc F() { _ = }\n", "package p\nfunc F() { _ = }\n"},
		{"package p\nfunc F() { _ = }\n", "package p\n"},
		{"package p\nfunc F() { _ = }\n", "package p\x00"},
		{"package p\x00", "package p\n"},
	} {
		after := []byte(test.after)
		count, checked := parseErrorCount("go", after)
		sum := blake3.Sum256(after)
		projection := &indexer.Projection{Language: "go", ContentHash: hex.EncodeToString(sum[:]), ParseChecked: checked, ParseErrors: count}
		require.Equal(t, checkParseGate("main.go", []byte(test.before), after), projectedParseGate("main.go", []byte(test.before), after, projection))
	}
}

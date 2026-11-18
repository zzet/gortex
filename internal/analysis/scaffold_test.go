package analysis

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"pgregory.net/rapid"
)

// --- Mock SourceReader ---

type mockSourceReader struct {
	g        *graph.Graph
	rootPath string
}

func (m *mockSourceReader) Graph() graph.Reader { return m.g }
func (m *mockSourceReader) ResolveFilePath(relPath string) string {
	if filepath.IsAbs(relPath) {
		return relPath
	}
	if m.rootPath == "" {
		return ""
	}
	return filepath.Join(m.rootPath, relPath)
}

// --- Helpers ---

// writeGoFile creates a Go source file in tmpDir at the given relative path
// with the provided content. Returns the relative path.
func writeGoFile(t *testing.T, tmpDir, relPath, content string) {
	t.Helper()
	absPath := filepath.Join(tmpDir, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o755))
	require.NoError(t, os.WriteFile(absPath, []byte(content), 0o644))
}

// Feature: gortex-enhancements, Property 7: Scaffold name substitution

// --- Generators ---

// genValidGoIdent generates a valid Go identifier (uppercase letter + lowercase suffix).
func genValidGoIdent() *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		upper := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J",
			"K", "L", "M", "N", "O", "P", "Q", "R", "S", "T", "U", "V", "W", "X", "Y", "Z"}
		lower := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j",
			"k", "l", "m", "n", "o", "p", "q", "r", "s", "t", "u", "v", "w", "x", "y", "z"}

		first := rapid.SampledFrom(upper).Draw(t, "first")
		suffixLen := rapid.IntRange(2, 8).Draw(t, "suffixLen")
		var sb strings.Builder
		sb.WriteString(first)
		for i := range suffixLen {
			sb.WriteString(rapid.SampledFrom(lower).Draw(t, fmt.Sprintf("s%d", i)))
		}
		return sb.String()
	})
}

// genDistinctIdentPair generates two distinct Go identifiers.
func genDistinctIdentPair() *rapid.Generator[[2]string] {
	return rapid.Custom(func(t *rapid.T) [2]string {
		a := genValidGoIdent().Draw(t, "identA")
		b := genValidGoIdent().Draw(t, "identB")
		// Ensure they are distinct
		for b == a {
			b = genValidGoIdent().Draw(t, "identBRetry")
		}
		return [2]string{a, b}
	})
}

// --- Property Tests ---

// TestPropertyScaffoldNameSubstitution verifies that for any existing symbol
// used as an example and for any valid new name, the scaffold output contains
// the new name in place of the example's name in all generated code, and each
// ScaffoldEdit has non-empty file_path, a positive insertion_line, and non-empty code.
func TestPropertyScaffoldNameSubstitution(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		names := genDistinctIdentPair().Draw(rt, "names")
		exampleName := names[0]
		newName := names[1]

		// Create a temp directory with a Go source file containing the example function
		tmpDir := t.TempDir()
		relPath := "pkg/example.go"
		sourceCode := fmt.Sprintf(`package pkg

func %s(x int) int {
	return x + 1
}
`, exampleName)
		writeGoFile(t, tmpDir, relPath, sourceCode)

		// Build a graph with the example function node
		g := graph.New()
		exampleID := fmt.Sprintf("%s::%s", relPath, exampleName)
		g.AddNode(&graph.Node{
			ID:        exampleID,
			Kind:      graph.KindFunction,
			Name:      exampleName,
			FilePath:  relPath,
			StartLine: 3,
			EndLine:   5,
			Language:  "go",
		})

		engine := query.NewEngine(g)
		reader := &mockSourceReader{g: g, rootPath: tmpDir}

		result, err := GenerateScaffold(engine, reader, exampleID, newName)
		if err != nil {
			rt.Fatalf("GenerateScaffold returned error: %v", err)
		}

		if len(result.Edits) == 0 {
			rt.Fatal("expected at least one ScaffoldEdit, got none")
		}

		// Verify each edit has valid fields and the new name is present
		foundNewName := false
		for i, edit := range result.Edits {
			if edit.FilePath == "" {
				rt.Errorf("edit[%d].FilePath is empty", i)
			}
			if edit.InsertionLine <= 0 {
				rt.Errorf("edit[%d].InsertionLine = %d, want > 0", i, edit.InsertionLine)
			}
			if edit.Code == "" {
				rt.Errorf("edit[%d].Code is empty", i)
			}
			if strings.Contains(edit.Code, newName) {
				foundNewName = true
			}
		}

		if !foundNewName {
			rt.Errorf("no edit contains the new name %q; edits: %v", newName, result.Edits)
		}

		// The new_symbol edit should NOT contain the old example name as a
		// standalone identifier. A simple strings.Contains would mis-flag
		// the new name when it's a superstring (e.g. example="Bbb",
		// new="Bbba" — "Bbba" trivially contains "Bbb"). Use a word-
		// boundary match so "Bbb" only counts when it stands alone.
		oldRE := regexp.MustCompile(`\b` + regexp.QuoteMeta(exampleName) + `\b`)
		for _, edit := range result.Edits {
			if edit.Reason == "new_symbol" {
				if oldRE.MatchString(edit.Code) {
					rt.Errorf("new_symbol edit still contains old name %q", exampleName)
				}
			}
		}
	})
}

// --- Unit Tests for Edge Cases ---

// TestScaffoldNoCallersOmitsRegistration verifies that when the example symbol
// has no callers at depth 1, the registration/wiring section is omitted and a
// note is included explaining that no wiring pattern was detected.
func TestScaffoldNoCallersOmitsRegistration(t *testing.T) {
	tmpDir := t.TempDir()
	relPath := "pkg/lonely.go"
	sourceCode := `package pkg

func LonelyFunc(x int) int {
	return x * 2
}
`
	writeGoFile(t, tmpDir, relPath, sourceCode)

	g := graph.New()
	exampleID := "pkg/lonely.go::LonelyFunc"
	g.AddNode(&graph.Node{
		ID:        exampleID,
		Kind:      graph.KindFunction,
		Name:      "LonelyFunc",
		FilePath:  relPath,
		StartLine: 3,
		EndLine:   5,
		Language:  "go",
	})
	// No callers added — the function is isolated

	engine := query.NewEngine(g)
	reader := &mockSourceReader{g: g, rootPath: tmpDir}

	result, err := GenerateScaffold(engine, reader, exampleID, "NewFunc")
	require.NoError(t, err)

	// Should have no registration edit
	for _, edit := range result.Edits {
		assert.NotEqual(t, "registration", edit.Reason,
			"registration edit should be omitted when there are no callers")
	}

	// Should have a note about missing callers
	foundNote := false
	for _, note := range result.Notes {
		if strings.Contains(note, "no callers") || strings.Contains(note, "registration") || strings.Contains(note, "wiring") {
			foundNote = true
			break
		}
	}
	assert.True(t, foundNote, "expected a note about omitted registration; notes: %v", result.Notes)
}

// TestScaffoldNoTestsOmitsTestStub verifies that when the example symbol has
// no associated test file or test function, the test stub is omitted and a
// note is included explaining that no test pattern was detected.
func TestScaffoldNoTestsOmitsTestStub(t *testing.T) {
	tmpDir := t.TempDir()
	relPath := "pkg/notested.go"
	sourceCode := `package pkg

func NoTestedFunc(s string) string {
	return s + "!"
}
`
	writeGoFile(t, tmpDir, relPath, sourceCode)
	// Deliberately do NOT create a _test.go file

	g := graph.New()
	exampleID := "pkg/notested.go::NoTestedFunc"
	g.AddNode(&graph.Node{
		ID:        exampleID,
		Kind:      graph.KindFunction,
		Name:      "NoTestedFunc",
		FilePath:  relPath,
		StartLine: 3,
		EndLine:   5,
		Language:  "go",
	})

	engine := query.NewEngine(g)
	reader := &mockSourceReader{g: g, rootPath: tmpDir}

	result, err := GenerateScaffold(engine, reader, exampleID, "NewFunc")
	require.NoError(t, err)

	// Should have no test_stub edit
	for _, edit := range result.Edits {
		assert.NotEqual(t, "test_stub", edit.Reason,
			"test_stub edit should be omitted when there is no test file")
	}

	// Should have a note about missing test pattern
	foundNote := false
	for _, note := range result.Notes {
		if strings.Contains(note, "no test") || strings.Contains(note, "test stub") || strings.Contains(note, "test pattern") {
			foundNote = true
			break
		}
	}
	assert.True(t, foundNote, "expected a note about omitted test stub; notes: %v", result.Notes)
}

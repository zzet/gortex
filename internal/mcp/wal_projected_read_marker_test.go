package mcp

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The C14 source reader confines its physical I/O and pairs current bytes with
// current ranges. The WAL readback marker must follow that reader without
// restoring the older direct os.ReadFile path or covering projection parsing.
func TestEditedFileMarkerSurvivesConfinedCurrentSourceReader(t *testing.T) {
	s, root := setupTestServer(t)
	spy := &interactiveReadStoreSpy{Store: s.graph, entered: make(chan struct{})}
	s.graph = spy
	s.sessions = newSessionMap()
	editor := WithSessionID(t.Context(), "editor")
	other := WithSessionID(t.Context(), "other")
	path := filepath.Join(root, "main.go")
	node := s.engineFor(editor).GetSymbol("main.go::helper")
	require.NotNil(t, node)
	require.Equal(t, 11, node.StartLine)
	shiftTestServerMain(t, root)
	s.rememberInteractiveEdit(editor, path)

	// A foreign session reads the new bytes/range without consuming the owner.
	current, err := s.readNodeSource(other, node, path)
	require.NoError(t, err)
	require.Equal(t, 13, current.startLine)
	select {
	case <-spy.entered:
		t.Fatal("another session borrowed interactive readback priority")
	default:
	}

	current, err = s.readNodeSource(editor, node, path)
	require.NoError(t, err)
	require.Equal(t, 13, current.startLine)
	require.Equal(t, "func helper() {}", strings.Split(current.content, "\n")[current.startLine-1])
	select {
	case <-spy.entered:
	default:
		t.Fatal("the real confined current-source read lost its edit marker")
	}
	require.Zero(t, spy.active.Load(), "priority must end before the reader returns")
	require.Equal(t, 11, node.StartLine, "readback must not rewrite the canonical node")
	require.Equal(t, 11, s.graph.GetNode(node.ID).StartLine, "source read must not publish graph changes")
	release := s.beginEditedFileRead(editor, path)
	require.Zero(t, spy.active.Load(), "the completed readback must consume its one-shot marker")
	release()
}

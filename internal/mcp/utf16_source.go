package mcp

import (
	"errors"
	"io"
	"os"

	"github.com/zzet/gortex/internal/indexer"
)

// UTF-16 source handling for the read and edit paths (#812, #846).
//
// The indexer decodes UTF-16 sources before extraction, so a symbol
// declared in a UTF-16 file is discoverable. Without the same handling
// here the tools would disagree with the graph: discovery finds the
// symbol, reads return NUL-interleaved text, and edits fail with "not
// found" instead of naming the real cause. Reads serve the decoded text
// — the same decode the indexer's transform applies, so display always
// matches what extraction indexed; edits refuse, because a text splice
// into NUL-interleaved bytes cannot express what the decoded edit means.

// decodedSourceForRead transcodes src when it is confidently UTF-16 text,
// reporting whether a decode happened. Everything else passes through
// byte-for-byte.
func decodedSourceForRead(src []byte) ([]byte, bool) {
	if !indexer.LooksUTF16Source(src) {
		return src, false
	}
	return indexer.DecodeUTF16Source(src), true
}

// utf16DecodedOmission is the omission note reads attach when they served
// a decoded UTF-16 source, so the model knows the payload was transcoded
// and that the edit tools will refuse the file.
func utf16DecodedOmission() map[string]any {
	return omission("utf16_decoded",
		"source file is UTF-16; the content shown is the decoded UTF-8 text the indexer extracted — the raw file on disk is NUL-interleaved, and edit_file / edit_symbol refuse UTF-16 sources")
}

// fileLooksUTF16 reports whether the file at absPath is confidently UTF-16 —
// a bounded prefix read, enough for the BOM and the NUL-parity sniff. Used by
// handlers whose read helper hides the raw bytes.
func fileLooksUTF16(absPath string) bool {
	f, err := os.Open(absPath)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 8192)
	n, _ := io.ReadFull(f, buf)
	if n <= 0 {
		return false
	}
	return indexer.LooksUTF16Source(buf[:n])
}

// refuseUTF16Edit is the refusal edit_file / edit_symbol / batch_edit
// return for a UTF-16 source. The old behaviour failed with
// "old_string not found" and pointed the agent at the read tools — which
// serve the decoded text — a loop with no exit. The refusal names the
// encoding and the escape hatches.
func refuseUTF16Edit(tool, path string) error {
	return errors.New(tool + " refuses a UTF-16 source: " + path +
		" is UTF-16 on disk, and a text splice into its NUL-interleaved bytes would corrupt it. " +
		"Re-encode the file as UTF-8 (e.g. `iconv -f UTF-16LE -t UTF-8`), edit it externally, and let the watcher re-index.")
}

// guardUTF16SourceWrite is the shared write-path check (#846): every tool
// that commits bytes to a source file funnels through commitFileMutation,
// which calls it, so a text-splice writer added later refuses here too.
// Handlers whose pre-write matching would fail with a misleading message
// call it (or the content-based check) early instead, for the encoding-
// specific refusal before any work is planned.
func guardUTF16SourceWrite(tool, relPath, absPath string) error {
	if fileLooksUTF16(absPath) {
		return refuseUTF16Edit(tool, relPath)
	}
	return nil
}

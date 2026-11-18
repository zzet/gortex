package languages

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// pdfToUnicodeCMap is a minimal one-byte ToUnicode CMap mapping printable
// ASCII onto itself. A font carrying it and no /Encoding sends the PDF
// reader down its CMap-decoding path — the one whose per-`Tf` re-parse made
// issue #676's memory blow up.
const pdfToUnicodeCMap = `/CIDInit /ProcSet findresource begin
12 dict begin
begincmap
1 begincodespacerange
<00> <FF>
endcodespacerange
1 beginbfrange
<20> <7E> <0020>
endbfrange
endcmap
CMapName currentdict /CMap defineresource pop
end
end
`

// buildTestPDF renders a small but structurally real PDF: a catalog, a flat
// page tree, one Type1 font with a flate-compressed ToUnicode CMap, and one
// flate-compressed content stream per page produced by pageContent(i)
// (1-based). It exists so the tests exercise the real reader without a
// binary fixture checked into the repo. kidsOverride, when given, replaces
// the page tree's /Kids array body verbatim.
func buildTestPDF(t testing.TB, pages int, pageContent func(page int) string, kidsOverride ...string) []byte {
	t.Helper()
	deflate := func(s string) []byte {
		var b bytes.Buffer
		zw := zlib.NewWriter(&b)
		_, err := zw.Write([]byte(s))
		require.NoError(t, err)
		require.NoError(t, zw.Close())
		return b.Bytes()
	}

	// Object numbers: 1 catalog, 2 page tree, 3 font, 4 cmap, then a
	// (page, contents) pair per page.
	var objs [][]byte
	add := func(body []byte) { objs = append(objs, body) }
	stream := func(data []byte) []byte {
		return fmt.Appendf(nil, "<< /Length %d /Filter /FlateDecode >>\nstream\n%s\nendstream", len(data), data)
	}

	kids := make([]string, pages)
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", 5+2*i)
	}
	kidList := strings.Join(kids, " ")
	if len(kidsOverride) > 0 {
		kidList = kidsOverride[0]
	}
	add([]byte("<< /Type /Catalog /Pages 2 0 R >>"))
	add(fmt.Appendf(nil, "<< /Type /Pages /Kids [%s] /Count %d >>", kidList, pages))
	add([]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /ToUnicode 4 0 R >>"))
	add(stream(deflate(pdfToUnicodeCMap)))
	for i := range pages {
		add(fmt.Appendf(nil, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 6+2*i))
		add(stream(deflate(pageContent(i + 1))))
	}

	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, body := range objs {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return out.Bytes()
}

// runPDFStream extracts data through the streaming route and returns the
// emitted nodes.
func runPDFStream(t testing.TB, data []byte) []*graph.Node {
	t.Helper()
	var nodes []*graph.Node
	err := NewPDFExtractor().ExtractStream("doc.pdf", bytes.NewReader(data), int64(len(data)),
		func(n *graph.Node, _ []*graph.Edge) { nodes = append(nodes, n) })
	require.NoError(t, err)
	return nodes
}

func pdfPageNodes(nodes []*graph.Node) []*graph.Node {
	var out []*graph.Node
	for _, n := range nodes {
		if n.Kind == graph.KindDoc {
			out = append(out, n)
		}
	}
	return out
}

// TestPDFExtractor_ExtractsPageText pins the basic contract on a generated
// PDF: one KindDoc per page, decoded through the font's ToUnicode CMap and
// whitespace-collapsed, on both the byte and streaming routes.
func TestPDFExtractor_ExtractsPageText(t *testing.T) {
	data := buildTestPDF(t, 3, func(page int) string {
		return fmt.Sprintf("BT /F1 12 Tf 72 700 Td (Hello   page) Tj T* [(number ) -250 (%d)] TJ ET", page)
	})

	for name, nodes := range map[string][]*graph.Node{
		"stream": runPDFStream(t, data),
		"bytes": func() []*graph.Node {
			res, err := NewPDFExtractor().Extract("doc.pdf", data)
			require.NoError(t, err)
			return res.Nodes
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, graph.KindFile, nodes[0].Kind)
			require.Equal(t, 3, nodes[0].Meta["pages"])
			pages := pdfPageNodes(nodes)
			require.Len(t, pages, 3)
			for i, p := range pages {
				require.Equal(t, i+1, p.Meta["page"])
				require.Equal(t, fmt.Sprintf("Hello page number %d", i+1), p.Meta["section_text"])
			}
		})
	}
}

// TestPDFExtractor_FontSwitchesStayBounded is the #676 regression: a page
// that re-selects its font many times (common in real-world PDFs — every
// styled run emits a `Tf`) used to re-parse the font's ToUnicode CMap on
// every switch, allocating a fresh zlib/flate decoder each time. A few MB
// of such PDFs drove the daemon into multi-GB RSS. Extraction must cost a
// small, bounded multiple of the document, not of its operator count.
func TestPDFExtractor_FontSwitchesStayBounded(t *testing.T) {
	const pages, switches = 4, 400
	data := buildTestPDF(t, pages, func(int) string {
		var b strings.Builder
		b.WriteString("BT\n")
		for range switches {
			b.WriteString("/F1 10 Tf (ab) Tj\n")
		}
		b.WriteString("ET\n")
		return b.String()
	})

	var nodes []*graph.Node
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	nodes = runPDFStream(t, data)
	runtime.ReadMemStats(&after)

	pagesOut := pdfPageNodes(nodes)
	require.Len(t, pagesOut, pages)
	require.Equal(t, strings.Repeat("ab", switches), pagesOut[0].Meta["section_text"])

	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("document %d bytes, extraction allocated %d bytes", len(data), allocated)
	const budget = 16 << 20
	require.Lessf(t, allocated, uint64(budget),
		"extracting a %d-byte PDF allocated %d bytes; font switches must not re-parse the CMap", len(data), allocated)
}

// TestPDFExtractor_TextCaps pins the per-page and per-document text bounds
// and that the page walk stops at pdfMaxPages.
func TestPDFExtractor_TextCaps(t *testing.T) {
	t.Run("page", func(t *testing.T) {
		long := strings.Repeat("word ", 2*pdfPageTextCap)
		data := buildTestPDF(t, 1, func(int) string { return "BT /F1 12 Tf (" + long + ") Tj ET" })
		pages := pdfPageNodes(runPDFStream(t, data))
		require.Len(t, pages, 1)
		text := pages[0].Meta["section_text"].(string)
		require.LessOrEqual(t, len(text), pdfPageTextCap)
		require.Greater(t, len(text), pdfPageTextCap-len("word "))
	})

	t.Run("document", func(t *testing.T) {
		long := strings.Repeat("x", pdfPageTextCap)
		n := pdfDocTextCap/pdfPageTextCap + 10
		data := buildTestPDF(t, n, func(int) string { return "BT /F1 12 Tf (" + long + ") Tj ET" })
		nodes := runPDFStream(t, data)
		total := 0
		for _, p := range pdfPageNodes(nodes) {
			total += len(p.Meta["section_text"].(string))
		}
		require.LessOrEqual(t, total, pdfDocTextCap)
		require.Equal(t, true, nodes[0].Meta["text_truncated"])
		require.Equal(t, n, nodes[0].Meta["pages"])
	})

	t.Run("pages", func(t *testing.T) {
		data := buildTestPDF(t, pdfMaxPages+5, func(int) string { return "BT /F1 12 Tf (p) Tj ET" })
		nodes := runPDFStream(t, data)
		require.Equal(t, pdfMaxPages, len(pdfPageNodes(nodes)))
		require.Equal(t, true, nodes[0].Meta["text_truncated"])
	})
}

// TestPDFExtractor_PageTreeCycle guards the page-tree walk against a
// malformed tree whose intermediate node lists itself as a kid: the walk
// must terminate instead of recursing forever.
func TestPDFExtractor_PageTreeCycle(t *testing.T) {
	// The tree's single intermediate node lists itself twice before the
	// real page: a naive recursive walk never terminates (and branches).
	data := buildTestPDF(t, 1, func(int) string { return "BT /F1 12 Tf (ok) Tj ET" }, "2 0 R 2 0 R 5 0 R")

	var nodes []*graph.Node
	require.NotPanics(t, func() { nodes = runPDFStream(t, data) })
	require.Equal(t, graph.KindFile, nodes[0].Kind)
	require.LessOrEqual(t, len(pdfPageNodes(nodes)), pdfMaxPages)
}

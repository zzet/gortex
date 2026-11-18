package languages

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	// Raster decoders registered for image.DecodeConfig — stdlib formats
	// plus the x/image extras already vendored. DecodeConfig reads only
	// the header, so this is cheap even for large images.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/ledongthuc/pdf"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// --- DCA10: multimodal ingest (PDF + images) -----------------------
//
// Code and markdown were the only file classes the graph ingested. The
// two extractors below make image assets and PDF documents first-class
// nodes: an image becomes a KindImage asset node carrying its format and
// dimensions, and a PDF becomes a KindFile node with one searchable
// KindDoc node per page, so a diagram or a spec PDF is discoverable
// alongside the code it documents.

// ImageAssetExtractor ingests raster / vector image files as graph nodes.
type ImageAssetExtractor struct{}

func NewImageAssetExtractor() *ImageAssetExtractor { return &ImageAssetExtractor{} }

func (e *ImageAssetExtractor) Language() string { return "image" }
func (e *ImageAssetExtractor) Extensions() []string {
	return []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".tiff", ".tif", ".ico", ".svg"}
}
func (e *ImageAssetExtractor) AssetClass() parser.AssetClass { return parser.AssetImage }

var _ parser.AssetExtractor = (*ImageAssetExtractor)(nil)

var svgOpenRe = regexp.MustCompile(`(?is)<svg\b[^>]*>`)
var svgAttrRe = regexp.MustCompile(`(?i)\b(width|height)\s*=\s*["']?\s*([0-9.]+)`)
var svgViewBoxRe = regexp.MustCompile(`(?is)\bviewBox\s*=\s*["']\s*[0-9.]+\s+[0-9.]+\s+([0-9.]+)\s+([0-9.]+)`)

func (e *ImageAssetExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	result := &parser.ExtractionResult{}
	base := path.Base(filePath)
	ext := strings.ToLower(path.Ext(filePath))
	format := strings.TrimPrefix(ext, ".")

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, Language: "image",
	}
	result.Nodes = append(result.Nodes, fileNode)

	sum := sha256.Sum256(src)
	meta := map[string]any{
		"asset_kind": "image",
		"format":     format,
		"size_bytes": len(src),
		"sha256":     hex.EncodeToString(sum[:]),
	}
	var w, h int
	if ext == ".svg" {
		w, h = svgDimensions(src)
		format = "svg"
	} else if cfg, decFormat, err := image.DecodeConfig(bytes.NewReader(src)); err == nil {
		w, h = cfg.Width, cfg.Height
		if decFormat != "" {
			format = decFormat
		}
	}
	meta["format"] = format
	if w > 0 {
		meta["width"] = w
	}
	if h > 0 {
		meta["height"] = h
	}

	imgID := "image::asset::" + filePath
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: imgID, Kind: graph.KindImage, Name: base,
		FilePath: filePath, StartLine: 1, Language: "image", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: filePath, To: imgID, Kind: graph.EdgeDefines, FilePath: filePath, Line: 1,
	})
	return result, nil
}

// svgDimensions parses an SVG's intrinsic size from the width / height
// attributes of its opening <svg> tag, falling back to the viewBox's
// w/h. Returns 0,0 when absent.
func svgDimensions(src []byte) (w, h int) {
	tag := svgOpenRe.Find(src)
	if tag == nil {
		return 0, 0
	}
	for _, m := range svgAttrRe.FindAllSubmatch(tag, -1) {
		v := atoiFloor(string(m[2]))
		switch strings.ToLower(string(m[1])) {
		case "width":
			if w == 0 {
				w = v
			}
		case "height":
			if h == 0 {
				h = v
			}
		}
	}
	if w == 0 || h == 0 {
		if m := svgViewBoxRe.FindSubmatch(tag); m != nil {
			if w == 0 {
				w = atoiFloor(string(m[1]))
			}
			if h == 0 {
				h = atoiFloor(string(m[2]))
			}
		}
	}
	return w, h
}

// atoiFloor parses the leading integer part of a numeric string, ignoring
// a fractional part and any trailing unit (e.g. "48px" -> 48, "12.5" -> 12).
func atoiFloor(s string) int {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, _ := strconv.Atoi(s[:end])
	return n
}

// PDFExtractor ingests PDF documents: one KindFile node carrying page
// count + size, plus one KindDoc node per page whose extracted text feeds
// the prose search index.
type PDFExtractor struct{}

func NewPDFExtractor() *PDFExtractor { return &PDFExtractor{} }

func (e *PDFExtractor) Language() string              { return "pdf" }
func (e *PDFExtractor) Extensions() []string          { return []string{".pdf"} }
func (e *PDFExtractor) AssetClass() parser.AssetClass { return parser.AssetDocument }

// Extraction bounds. A PDF's cost must stay a small multiple of its size no
// matter what its page tree or content streams look like (#676), so every
// dimension the reader walks is capped and extraction stops early — keeping
// what it already has — once a cap is reached.
const (
	// pdfPageTextCap bounds the per-page text stored on a KindDoc node so a
	// large PDF can't bloat the graph; the head of each page is enough for
	// prose search to locate the right document.
	pdfPageTextCap = 4000
	// pdfDocTextCap bounds the text kept across all pages of one document.
	pdfDocTextCap = 1 << 20
	// pdfMaxPages bounds how many pages are extracted from one document.
	pdfMaxPages = 1000
	// pdfMaxTreeNodes bounds the page-tree nodes visited while locating
	// pages, so a malformed (e.g. self-referencing) tree can't loop forever.
	pdfMaxTreeNodes = 4 * pdfMaxPages
	// pdfMaxTreeDepth bounds page-tree nesting; real trees are a few levels.
	pdfMaxTreeDepth = 32
	// pdfPageOpBudget bounds the content-stream operators interpreted per
	// page, so a page of vector art with no text can't burn unbounded work.
	pdfPageOpBudget = 200_000
)

// compile-time guarantee that PDFExtractor offers the streaming route the
// indexer prefers (one page at a time, never the whole file in memory), and
// that it is gated as a document asset.
var (
	_ parser.StreamingExtractor = (*PDFExtractor)(nil)
	_ parser.AssetExtractor     = (*PDFExtractor)(nil)
)

func (e *PDFExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	result := &parser.ExtractionResult{}
	sum := sha256.Sum256(src)
	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, Language: "pdf",
		Meta: map[string]any{
			"asset_kind": "pdf",
			"data_class": "content",
			"size_bytes": len(src),
			"sha256":     hex.EncodeToString(sum[:]),
		},
	}
	result.Nodes = append(result.Nodes, fileNode)

	// The PDF reader can panic on malformed / encrypted documents — keep
	// extraction failures from killing the whole index by recovering and
	// returning just the file node.
	func() {
		defer func() { _ = recover() }()
		r, err := pdf.NewReader(bytes.NewReader(src), int64(len(src)))
		if err != nil || r == nil {
			return
		}
		pdfEmitPages(filePath, r, fileNode, func(n *graph.Node, edges []*graph.Edge) {
			result.Nodes = append(result.Nodes, n)
			result.Edges = append(result.Edges, edges...)
		})
	}()
	return result, nil
}

// ExtractStream implements parser.StreamingExtractor: it reads the PDF through
// the supplied io.ReaderAt one page at a time, so a large document is never
// held whole in memory. It emits one KindFile node plus one KindDoc node per
// page that has extractable text. The sha256 carried by the byte-path Extract
// is omitted here — hashing would require a full read, defeating the stream.
func (e *PDFExtractor) ExtractStream(filePath string, r io.ReaderAt, size int64, emit func(*graph.Node, []*graph.Edge)) error {
	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, Language: "pdf",
		Meta: map[string]any{
			"asset_kind": "pdf",
			"data_class": "content",
			"size_bytes": int(size),
		},
	}
	emit(fileNode, nil)

	// The PDF reader can panic on malformed / encrypted documents — keep
	// extraction failures from killing the pass by recovering and emitting
	// just the file node.
	func() {
		defer func() { _ = recover() }()
		rd, err := pdf.NewReader(r, size)
		if err != nil || rd == nil {
			return
		}
		pdfEmitPages(filePath, rd, fileNode, emit)
	}()
	return nil
}

// pdfEmitPages walks the pages of r in document order, emitting one capped
// KindDoc node (plus its defines edge from the file) per page that has
// extractable text. It stops at pdfMaxPages pages or once pdfDocTextCap bytes
// of text are kept, marking fileNode.Meta["text_truncated"]. It stamps
// fileNode.Meta["pages"] as a side effect. Shared by Extract and ExtractStream.
func pdfEmitPages(filePath string, r *pdf.Reader, fileNode *graph.Node, emit func(*graph.Node, []*graph.Edge)) {
	if pages := r.NumPage(); pages > 0 {
		fileNode.Meta["pages"] = pages
	}
	budget := pdfDocTextCap
	encoders := make(map[string]pdf.TextEncoding)
	i := 0
	complete := pdfWalkPages(r.Trailer().Key("Root").Key("Pages"), func(p pdf.Page) bool {
		if i == pdfMaxPages || budget <= 0 {
			return false
		}
		i++
		text := pdfPageText(p, min(pdfPageTextCap, budget), encoders)
		if text == "" {
			return true
		}
		budget -= len(text)
		pageID := filePath + "::doc:page-" + strconv.Itoa(i)
		pageNode := &graph.Node{
			ID: pageID, Kind: graph.KindDoc, Name: path.Base(filePath) + " p." + strconv.Itoa(i),
			FilePath: filePath, StartLine: i, Language: "pdf",
			Meta: map[string]any{"asset_kind": "pdf_page", "data_class": "content", "page": i, "section_text": text},
		}
		emit(pageNode, []*graph.Edge{{
			From: filePath, To: pageID, Kind: graph.EdgeDefines, FilePath: filePath, Line: i,
		}})
		return true
	})
	if !complete {
		fileNode.Meta["text_truncated"] = true
	}
}

// pdfWalkPages visits the leaf pages of the page tree rooted at root in
// document order until visit returns false. It walks the tree once, rather
// than re-descending from the root per page as pdf.Reader.Page does — that is
// quadratic in the page count and never terminates on a self-referencing
// tree. The walk is bounded by pdfMaxTreeNodes and pdfMaxTreeDepth; it
// reports whether the whole tree was walked.
func pdfWalkPages(root pdf.Value, visit func(pdf.Page) bool) bool {
	visited := 0
	var walk func(v pdf.Value, depth int) bool
	walk = func(v pdf.Value, depth int) bool {
		visited++
		if visited > pdfMaxTreeNodes || depth > pdfMaxTreeDepth {
			return false
		}
		switch v.Key("Type").Name() {
		case "Pages":
			kids := v.Key("Kids")
			for k := 0; k < kids.Len(); k++ {
				if !walk(kids.Index(k), depth+1) {
					return false
				}
			}
		case "Page":
			return visit(pdf.Page{V: v})
		}
		return true
	}
	return walk(root, 0)
}

// pdfStop is panicked from inside pdf.Interpret — which offers no other way
// to stop early — once a page reaches its text or operator budget.
// pdfPageText recovers it and keeps the text gathered so far.
type pdfStop struct{}

// pdfRawEncoding passes bytes through undecoded, for text shown in a font
// the page doesn't declare (pdf's own nopEncoder is unexported).
type pdfRawEncoding struct{}

func (pdfRawEncoding) Decode(raw string) string { return raw }

// pdfPageText extracts one page's plain text, whitespace-collapsed and capped
// at limit bytes. It replaces pdf.Page.GetPlainText, whose font-encoder cache
// never takes effect (Font.Encoder has a value receiver), so every `Tf`
// operator re-parsed the font's ToUnicode CMap through a fresh flate decoder:
// a page switching fonts thousands of times allocated hundreds of MB (#676).
// Here each font's encoder is built once per document (docEncoders), and
// interpretation stops as soon as limit bytes of text or pdfPageOpBudget
// operators are reached. Any other reader panic yields "" for the page.
func pdfPageText(p pdf.Page, limit int, docEncoders map[string]pdf.TextEncoding) (text string) {
	var w pdfTextWriter
	w.limit = limit
	defer func() {
		if rec := recover(); rec != nil {
			text = ""
			if _, ok := rec.(pdfStop); ok {
				text = w.b.String()
			}
		}
	}()
	if p.V.IsNull() || limit <= 0 {
		return ""
	}
	contents := p.V.Key("Contents")
	if contents.Kind() == pdf.Null {
		return ""
	}
	fonts := p.Resources().Key("Font")
	pageEncoders := make(map[string]pdf.TextEncoding) // by resource name
	var enc pdf.TextEncoding = pdfRawEncoding{}
	var args []pdf.Value // operands, top of stack (last operand) first
	ops := 0
	pdf.Interpret(contents, func(stk *pdf.Stack, op string) {
		args = args[:0]
		for stk.Len() > 0 {
			args = append(args, stk.Pop())
		}
		if ops++; ops > pdfPageOpBudget {
			panic(pdfStop{})
		}
		switch op {
		case "BT", "T*": // new text object / next line
			w.space()
		case "Tf": // set font: name size
			if len(args) != 2 {
				return
			}
			name := args[1].Name()
			e, ok := pageEncoders[name]
			if !ok {
				e = pdfFontEncoder(fonts.Key(name), docEncoders)
				pageEncoders[name] = e
			}
			enc = e
		case "Tj", "'", "\"": // show text (the string is the last operand)
			if op != "Tj" {
				w.space()
			}
			if len(args) > 0 {
				w.write(enc.Decode(args[0].RawString()))
			}
		case "TJ": // show text with glyph positioning
			if len(args) != 1 {
				return
			}
			for k, arr := 0, args[0]; k < arr.Len(); k++ {
				if x := arr.Index(k); x.Kind() == pdf.String {
					w.write(enc.Decode(x.RawString()))
				}
			}
		}
	})
	return w.b.String()
}

// pdfMaxCachedEncoders bounds the per-document font-encoder cache.
const pdfMaxCachedEncoders = 256

// pdfFontEncoder returns the text encoder for a page's font dictionary,
// sharing it across the document's pages through cache. The cache key is the
// font dictionary's canonical rendering: indirect objects (the ToUnicode
// CMap stream among them) print as their object references, so equal keys
// mean the same font and therefore the same encoder. A missing font decodes
// as raw bytes, as pdf.Page.GetPlainText does.
func pdfFontEncoder(font pdf.Value, cache map[string]pdf.TextEncoding) pdf.TextEncoding {
	if font.IsNull() {
		return pdfRawEncoding{}
	}
	key := font.String()
	if e, ok := cache[key]; ok {
		return e
	}
	e := pdf.Font{V: font}.Encoder()
	if len(cache) < pdfMaxCachedEncoders {
		cache[key] = e
	}
	return e
}

// pdfTextWriter accumulates whitespace-collapsed text (the same result as
// strings.Join(strings.Fields(s), " ")) and panics pdfStop rather than grow
// past limit bytes.
type pdfTextWriter struct {
	b       strings.Builder
	limit   int
	pending bool // whitespace seen since the last written rune
}

func (w *pdfTextWriter) space() {
	if w.b.Len() > 0 {
		w.pending = true
	}
}

func (w *pdfTextWriter) write(s string) {
	for _, r := range s {
		if unicode.IsSpace(r) {
			w.space()
			continue
		}
		n := utf8.RuneLen(r)
		if w.pending {
			n++
		}
		if w.b.Len()+n > w.limit {
			panic(pdfStop{})
		}
		if w.pending {
			w.b.WriteByte(' ')
			w.pending = false
		}
		w.b.WriteRune(r)
	}
}

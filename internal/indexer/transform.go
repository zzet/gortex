package indexer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/platform"
)

// defaultTransformTimeout bounds a transform subprocess when the rule
// does not set one.
const defaultTransformTimeout = 30 * time.Second

// contentTransform rewrites a file's raw bytes before extraction.
type contentTransform interface {
	name() string
	matches(path string) bool
	apply(path string, src []byte) ([]byte, error)
	// asLanguage returns a non-empty language when this transform
	// re-types matching files to a language their extension does not
	// natively map to.
	asLanguage() string
}

// preParseTransform rewrites a file's bytes before extraction WITHOUT moving
// any byte — its output is the same length as its input, so every offset (and
// therefore every symbol's line/column) is preserved against the original
// file. This is the slot for neutralising a parser-hostile span: blanking it
// to spaces lets the grammar parse the surrounding code while positions stay
// exact. It runs ahead of the offset-shifting contentTransforms (BOM strip,
// command rewrites), so a pre-parse transform always sees the original layout.
type preParseTransform interface {
	name() string
	matches(path string) bool
	// rewrite returns the transformed bytes. It MUST return a slice the same
	// length as src; the pipeline discards (and logs) a length-changing result
	// so the offset-preservation guarantee can never be silently violated.
	rewrite(path string, src []byte) []byte
}

// transformPipeline applies an ordered list of content transforms to a
// file's bytes before the parser sees them. The offset-preserving prePass
// slot runs first, then the offset-shifting transforms.
type transformPipeline struct {
	prePass    []preParseTransform
	transforms []contentTransform
	logger     *zap.Logger
}

// newTransformPipeline builds the pipeline: the always-on BOM stripper
// followed by every user-declared external-command transform, in
// config order.
func newTransformPipeline(rules []config.TransformRule, logger *zap.Logger) *transformPipeline {
	if logger == nil {
		logger = zap.NewNop()
	}
	p := &transformPipeline{logger: logger}
	// Offset-preserving pre-parse slot: built-ins that blank parser-hostile
	// spans to spaces without shifting positions.
	p.prePass = append(p.prePass, csharpPreprocBlankTransform{})
	p.transforms = append(p.transforms, bomStripTransform{})
	for _, r := range rules {
		if len(r.Command) == 0 {
			logger.Warn("indexer: transform rule has no command; ignored",
				zap.String("name", r.Name))
			continue
		}
		p.transforms = append(p.transforms, newCommandTransform(r))
	}
	return p
}

// run applies every matching transform to src in order. A transform
// that errors is logged and skipped — the bytes from the previous
// stage are kept, so one failing processor never drops a file.
func (p *transformPipeline) run(path string, src []byte) []byte {
	if p == nil {
		return src
	}
	out := src
	// Offset-preserving pre-parse slot first: these neutralise parser-hostile
	// spans without moving any byte, so the offset-shifting transforms below
	// still see the original layout and positions stay exact.
	for _, t := range p.prePass {
		if !t.matches(path) {
			continue
		}
		res := t.rewrite(path, out)
		if len(res) != len(out) {
			p.logger.Warn("indexer: pre-parse transform changed length; dropped to preserve offsets",
				zap.String("transform", t.name()), zap.String("file", path),
				zap.Int("want_len", len(out)), zap.Int("got_len", len(res)))
			continue
		}
		out = res
	}
	for _, t := range p.transforms {
		if !t.matches(path) {
			continue
		}
		res, err := t.apply(path, out)
		if err != nil {
			p.logger.Warn("indexer: content transform failed; keeping untransformed bytes",
				zap.String("transform", t.name()), zap.String("file", path), zap.Error(err))
			continue
		}
		out = res
	}
	return out
}

// prepareCoordinateStable applies only source preparation whose output keeps
// a one-to-one byte and line mapping to the caller-owned bytes. Unindexed
// rename recovery edits the raw file, so an arbitrary command transform may
// not synthesize or relocate a declaration and still be advertised as a
// declaration-only fallback.
func (p *transformPipeline) prepareCoordinateStable(path string, src []byte) ([]byte, error) {
	if p == nil {
		return src, nil
	}
	out := src
	for _, transform := range p.prePass {
		if !transform.matches(path) {
			continue
		}
		rewritten := transform.rewrite(path, out)
		if !sameSourceCoordinates(out, rewritten) {
			return nil, fmt.Errorf("pre-parse transform %q does not preserve source coordinates", transform.name())
		}
		out = rewritten
	}
	for _, transform := range p.transforms {
		if !transform.matches(path) {
			continue
		}
		switch transform.(type) {
		case bomStripTransform:
			var err error
			out, err = neutralizeSourceBOM(out)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("source transform %q does not guarantee coordinate preservation", transform.name())
		}
	}
	return out, nil
}

func sameSourceCoordinates(raw, prepared []byte) bool {
	if len(raw) != len(prepared) {
		return false
	}
	for i := range raw {
		rawBreak := raw[i] == '\n' || raw[i] == '\r'
		preparedBreak := prepared[i] == '\n' || prepared[i] == '\r'
		if (rawBreak || preparedBreak) && raw[i] != prepared[i] {
			return false
		}
	}
	return true
}

func neutralizeSourceBOM(src []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(src, []byte{0xef, 0xbb, 0xbf}):
		out := bytes.Clone(src)
		copy(out[:3], []byte("   "))
		return out, nil
	case bytes.HasPrefix(src, []byte{0xff, 0xfe}), bytes.HasPrefix(src, []byte{0xfe, 0xff}):
		return nil, fmt.Errorf("UTF-16 BOM cannot be prepared without changing source coordinates")
	default:
		return src, nil
	}
}

// languageFor returns the language a transform re-types path to, or ""
// when no transform claims it. Lets a file whose extension is not
// natively indexed (e.g. .pdf) still reach an extractor.
func (p *transformPipeline) languageFor(path string) string {
	if p == nil {
		return ""
	}
	for _, t := range p.transforms {
		if t.asLanguage() != "" && t.matches(path) {
			return t.asLanguage()
		}
	}
	return ""
}

// sniffPrefixBytes bounds the prefix read for a shebang probe on a
// file whose extension the registry does not recognise.
const sniffPrefixBytes = 512

// readSniffPrefix reads up to sniffPrefixBytes from path for a content
// probe. Returns nil on any error — the caller treats a nil prefix as
// "no content available" and degrades to name-based detection.
//
// Under a content source the prefix comes out of the snapshot, so a file
// that is not in the working tree still gets its shebang probe. That read
// is drained with ReadFull because a source can be backed by a pipe,
// which answers a single Read with whatever happens to be buffered.
func (idx *Indexer) readSniffPrefix(path string) []byte {
	if src := idx.contentSource(); src != nil {
		rel, ok := idx.sourceRelPath(path)
		if !ok {
			return nil
		}
		rc, _, err := src.Open(rel)
		if err != nil {
			return nil
		}
		defer rc.Close()
		buf := make([]byte, sniffPrefixBytes)
		n, _ := io.ReadFull(rc, buf)
		if n <= 0 {
			return nil
		}
		return buf[:n]
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, sniffPrefixBytes)
	n, _ := f.Read(buf)
	if n <= 0 {
		return nil
	}
	return buf[:n]
}

// effectiveLanguage detects a file's language: its native extension
// mapping first, with a content probe disambiguating an ambiguous
// extension (.h, .m) when src is supplied; a `#!` shebang fallback for
// an unknown-extension script (reading a bounded prefix when the
// caller holds no content); then any transform rule that re-types it.
//
// src may be nil — callers that have not read the file (walk-time and
// staleness gates) pass nil and still get the shebang fallback via the
// bounded prefix read.
func (idx *Indexer) effectiveLanguage(path string, src []byte) (string, bool) {
	if lang, ok := idx.registry.DetectLanguageContent(path, src); ok {
		return lang, true
	}
	if len(src) == 0 {
		if prefix := idx.readSniffPrefix(path); prefix != nil {
			if lang, ok := idx.registry.DetectLanguageContent(path, prefix); ok {
				return lang, true
			}
		}
	}
	if lang := idx.transforms.languageFor(path); lang != "" {
		return lang, true
	}
	return "", false
}

// --- built-in: BOM strip -------------------------------------------------

// bomStripTransform removes a leading UTF-8 / UTF-16 byte-order mark. A
// BOM at offset 0 is not whitespace to a tree-sitter grammar and breaks
// the first token (e.g. a Go file's `package` clause), so stripping it
// is always correct — this transform is on for every file.
type bomStripTransform struct{}

func (bomStripTransform) name() string        { return "bom-strip" }
func (bomStripTransform) matches(string) bool { return true }
func (bomStripTransform) asLanguage() string  { return "" }
func (bomStripTransform) apply(_ string, src []byte) ([]byte, error) {
	return stripBOM(src), nil
}

// stripBOM drops a leading UTF-8, UTF-16LE or UTF-16BE byte-order mark.
func stripBOM(src []byte) []byte {
	switch {
	case len(src) >= 3 && src[0] == 0xEF && src[1] == 0xBB && src[2] == 0xBF:
		return src[3:]
	case len(src) >= 2 && src[0] == 0xFF && src[1] == 0xFE:
		return src[2:]
	case len(src) >= 2 && src[0] == 0xFE && src[1] == 0xFF:
		return src[2:]
	default:
		return src
	}
}

// --- user-pluggable: external command ------------------------------------

// commandTransform pipes a file's content through an external program:
// content in on stdin, transformed content out on stdout.
type commandTransform struct {
	rname   string
	exts    map[string]bool
	argv    []string
	asLang  string
	timeout time.Duration
}

func newCommandTransform(r config.TransformRule) *commandTransform {
	exts := make(map[string]bool, len(r.Extensions))
	for _, e := range r.Extensions {
		exts[strings.ToLower(e)] = true
	}
	timeout := defaultTransformTimeout
	if r.TimeoutMillis > 0 {
		timeout = time.Duration(r.TimeoutMillis) * time.Millisecond
	}
	name := r.Name
	if name == "" {
		name = r.Command[0]
	}
	return &commandTransform{
		rname:   name,
		exts:    exts,
		argv:    append([]string(nil), r.Command...),
		asLang:  r.AsLanguage,
		timeout: timeout,
	}
}

func (c *commandTransform) name() string       { return c.rname }
func (c *commandTransform) asLanguage() string { return c.asLang }

func (c *commandTransform) matches(path string) bool {
	if len(c.exts) == 0 {
		return true
	}
	return c.exts[strings.ToLower(filepath.Ext(path))]
}

func (c *commandTransform) apply(_ string, src []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	// argv is operator-declared config, not user-derived input.
	cmd := exec.CommandContext(ctx, c.argv[0], c.argv[1:]...) //nolint:gosec
	platform.ConfigureBackgroundCommand(cmd)
	cmd.Stdin = bytes.NewReader(src)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if stderr := strings.TrimSpace(errBuf.String()); stderr != "" {
			return nil, fmt.Errorf("%w: %s", err, stderr)
		}
		return nil, err
	}
	return out.Bytes(), nil
}

// --- built-in pre-parse: C# conditional-compilation directive blank ------

// csharpPreprocRe matches a C# conditional-compilation directive line —
// `#if` / `#elif` / `#else` / `#endif`, the first non-space token on its line
// (a C# requirement, so anchored to line start) through end of line. The
// structural directives `#region` / `#pragma` / `#nullable` / `#define` parse
// fine and are deliberately left alone.
var csharpPreprocRe = regexp.MustCompile(`(?m)^[ \t]*#[ \t]*(?:if|elif|else|endif)\b[^\n]*`)

// csharpPreprocBlankTransform blanks C# conditional-compilation directive
// lines to spaces before parsing. The shipped tree-sitter C# grammar
// mis-parses some preprocessor forms; blanking the directive lines — while
// keeping the guarded code on both branches of an `#if/#else`, the right
// default for a code graph that indexes every symbol regardless of build
// flags — sidesteps it. Replacement is space-for-character with tabs and
// newlines kept, so the file's length and every symbol's line/column stay
// exact: an offset-preserving pre-parse transform.
type csharpPreprocBlankTransform struct{}

func (csharpPreprocBlankTransform) name() string { return "csharp-preproc-blank" }

func (csharpPreprocBlankTransform) matches(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".cs")
}

func (csharpPreprocBlankTransform) rewrite(_ string, src []byte) []byte {
	return blankCSharpPreprocDirectives(src)
}

// blankCSharpPreprocDirectives replaces every conditional-compilation
// directive line's characters with spaces (tabs and newlines preserved),
// keeping the byte length identical. Returns src unchanged when it holds no
// `#` at all.
func blankCSharpPreprocDirectives(src []byte) []byte {
	if !bytes.ContainsRune(src, '#') {
		return src
	}
	return csharpPreprocRe.ReplaceAllFunc(src, func(m []byte) []byte {
		out := make([]byte, len(m))
		for i, b := range m {
			if b == '\t' {
				out[i] = '\t' // keep tab columns
			} else {
				out[i] = ' '
			}
		}
		return out
	})
}

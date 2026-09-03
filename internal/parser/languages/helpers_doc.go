package languages

import (
	"bytes"
	"strings"
)

// docMaxLen caps the stored doc comment length. 400 chars covers a
// typical first paragraph (~80 tokens) without bloating GCX1
// responses.
const docMaxLen = 400

// docCommentLang controls which prefix-strip rule the helper applies.
// The helper is reused across languages with the same line-comment
// syntax but different leading-prefix conventions.
type docCommentLang int

const (
	// DocLangSlashSlash strips a leading "//" (and optional space).
	// Captures Go, JavaScript, TypeScript, Rust (///, //!), C/C++,
	// C#, Swift, Dart, Kotlin, Scala line comments.
	DocLangSlashSlash docCommentLang = iota
	// DocLangBlockStar handles JSDoc/Javadoc/PHPDoc style /** ... */
	// blocks plus a fallback to // single-line comments above the
	// declaration.
	DocLangBlockStar
	// DocLangHash strips a leading "#" (and optional space).
	// Captures Python (line comments above defs), Ruby, Bash, R,
	// Makefile.
	DocLangHash
	// DocLangCSharpXML strips C# XML doc markers (/// <summary>...).
	DocLangCSharpXML
	// DocLangDashDash strips a leading "--" (and optional space). Captures
	// Lua (-- / ---), SQL, Haskell, and Ada line comments above a
	// declaration. Pascal/Delphi `//` doc comments are served by
	// DocLangSlashSlash; this adds the dash-comment family.
	DocLangDashDash
)

// docWrapperTokens are the modifier tokens a "wrapper" line may consist
// of — a line that sits between a doc comment and the real declaration
// carrying nothing but export / visibility modifiers (`export default`,
// a bare `public`).
var docWrapperTokens = map[string]bool{
	"export": true, "default": true,
	"public": true, "private": true, "protected": true, "internal": true, "pub": true,
	"abstract": true, "final": true, "static": true,
}

// isDocWrapperLine reports whether a non-comment line is a declaration wrapper
// the doc-comment climb should skip over (rather than terminate on) when no
// comment has been collected yet: a decorator / annotation (`@Component`,
// `@app.route(...)`, `@dataclass`) or a line made ONLY of export /
// visibility modifiers that sit on their own line between the doc and the
// declaration.
//
// A line that merely STARTS with a modifier is not a wrapper — it is the
// previous declaration. Climbing past `public int A { get; set; }` handed
// A's doc to every undocumented member below it (any language whose
// members lead with a modifier), and made the climb linear in the member
// count per member, quadratic per file.
func isDocWrapperLine(trimmed []byte) bool {
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] == '@' { // decorator / annotation
		return true
	}
	// Token scan in place — no bytes.Fields slice — so ExtractDocAbove's
	// allocation contract still holds on this path.
	rest := trimmed
	for len(rest) > 0 {
		rest = bytes.TrimLeft(rest, " \t")
		if len(rest) == 0 {
			break
		}
		end := bytes.IndexAny(rest, " \t")
		if end < 0 {
			end = len(rest)
		}
		if !docWrapperTokens[string(rest[:end])] {
			return false
		}
		rest = rest[end:]
	}
	return true
}

// ExtractDocAbove walks upward from startRow0 collecting contiguous
// comment lines that sit above the declaration, and returns the first
// paragraph as a single line, truncated to docMaxLen. startRow0 is the
// 0-based row of the declaration's first line (matching tree-sitter's
// row numbering).
//
// "Contiguous" means no blank line and no non-comment line between the
// last collected comment line and the declaration. A blank or
// non-comment line terminates the scan upward.
//
// Returns "" when no leading comment is found. Safe to call on every
// emit — the cost per call is O(comment-block-size).
//
// Allocation contract: the only allocations are the small `collected`
// slice plus the trimmed-prefix strings returned by stripLineComment*.
// The line walk uses bytes.LastIndexByte against src in place — no
// intermediate `[][]byte` of every preceding line is built. The
// `make([][]byte, 0, upToRow)` predecessor was 37% of the indexer's
// total allocation on a TS-heavy repo (Profile #4).
func ExtractDocAbove(src []byte, startRow0 int, lang docCommentLang) string {
	if startRow0 <= 0 || len(src) == 0 {
		return ""
	}
	// Locate the byte offset of the '\n' that ends row (startRow0 - 1).
	// Lines we want are rows [0, startRow0); the last such line ends at
	// the byte just before this '\n' (exclusive of the '\n' itself).
	//
	// If the forward scan exhausts src without reaching startRow0
	// (startRow0 is past EOF, or the trailing row has no '\n'), `end`
	// retains the last seen '\n'. The trailing partial line with no
	// terminator is intentionally not walked — this matches the
	// behaviour of the predecessor `lineBytesUpTo`, which only
	// appended a line when it observed its terminating '\n', so a
	// missing-newline final row was dropped.
	end := -1
	{
		row := 0
		for i := 0; i < len(src); i++ {
			if src[i] != '\n' {
				continue
			}
			row++
			end = i
			if row == startRow0 {
				break
			}
		}
	}
	return extractDocAboveEnd(src, end, lang)
}

// ExtractDocAboveByte is ExtractDocAbove addressed by the declaration's
// start BYTE offset instead of its row. The row form has to count
// newlines from the top of the file to find its starting point, which
// is linear in the declaration's position and therefore quadratic over a
// file's members; a caller holding the tree-sitter node can hand over
// its start byte and the walk begins right there. Same lines, same
// result.
func ExtractDocAboveByte(src []byte, startByte int, lang docCommentLang) string {
	if startByte <= 0 || len(src) == 0 {
		return ""
	}
	if startByte > len(src) {
		startByte = len(src)
	}
	return extractDocAboveEnd(src, bytes.LastIndexByte(src[:startByte], '\n'), lang)
}

// extractDocAboveEnd is the shared upward walk. end indexes the '\n'
// that terminates the line directly above the declaration; -1 means
// there is no such line.
func extractDocAboveEnd(src []byte, end int, lang docCommentLang) string {
	if end < 0 {
		return ""
	}

	// Single-byte byte-slice literals used by the inner loop — hoisted
	// so the bytes.HasPrefix / HasSuffix / TrimPrefix / TrimSuffix
	// calls don't reallocate them on every iteration. Tiny, but the
	// loop runs in the indexer's hottest path.
	var (
		slashSlash  = []byte("//")
		hash        = []byte("#")
		tripleSlash = []byte("///")
		blockEnd    = []byte("*/")
		blockStart  = []byte("/**")
		dashDash    = []byte("--")
	)

	// Walk upward from the line just above the declaration. `end`
	// indexes the '\n' that terminates the current line (exclusive);
	// the line's bytes are src[prev+1 : end] where `prev` is the '\n'
	// that ends the previous line, or -1 when we hit row 0.
	collected := make([]string, 0, 8)
	inBlock := false
	for end >= 0 {
		prev := bytes.LastIndexByte(src[:end], '\n')
		line := bytes.TrimRight(src[prev+1:end], "\r")
		trimmed := bytes.TrimSpace(line)
		// Step now so the existing `continue` statements in the switch
		// advance to the previous line. `goto done` skips the step
		// entirely, which is what we want.
		end = prev

		switch lang {
		case DocLangSlashSlash:
			if len(trimmed) == 0 {
				if len(collected) == 0 {
					continue
				}
				goto done
			}
			if bytes.HasPrefix(trimmed, slashSlash) {
				collected = append(collected, stripLineCommentPrefixBytes(trimmed, slashSlash))
				continue
			}
			if len(collected) == 0 && isDocWrapperLine(trimmed) {
				continue // climb past a decorator / export wrapper to the doc
			}
			goto done

		case DocLangBlockStar:
			// Match `*/` end → walk into block. Match `/**` start →
			// finish block. Otherwise treat as // line comments
			// fallback.
			if !inBlock && bytes.HasSuffix(trimmed, blockEnd) {
				body := bytes.TrimSuffix(trimmed, blockEnd)
				body = bytes.TrimSpace(body)
				if bytes.HasPrefix(trimmed, blockStart) {
					// Single-line /** ... */ block.
					inner := bytes.TrimPrefix(body, blockStart)
					inner = bytes.TrimSpace(inner)
					if len(inner) != 0 {
						collected = append(collected, string(inner))
					}
					goto done
				}
				inBlock = true
				if len(body) != 0 {
					collected = append(collected, stripBlockStarLineBytes(body))
				}
				continue
			}
			if inBlock {
				if bytes.HasPrefix(trimmed, blockStart) {
					body := bytes.TrimPrefix(trimmed, blockStart)
					body = bytes.TrimSpace(body)
					if len(body) != 0 {
						collected = append(collected, stripBlockStarLineBytes(body))
					}
					goto done
				}
				collected = append(collected, stripBlockStarLineBytes(trimmed))
				continue
			}
			if len(trimmed) == 0 {
				if len(collected) == 0 {
					continue
				}
				goto done
			}
			if bytes.HasPrefix(trimmed, slashSlash) {
				collected = append(collected, stripLineCommentPrefixBytes(trimmed, slashSlash))
				continue
			}
			if len(collected) == 0 && isDocWrapperLine(trimmed) {
				continue
			}
			goto done

		case DocLangHash:
			if len(trimmed) == 0 {
				if len(collected) == 0 {
					continue
				}
				goto done
			}
			if bytes.HasPrefix(trimmed, hash) {
				collected = append(collected, stripLineCommentPrefixBytes(trimmed, hash))
				continue
			}
			if len(collected) == 0 && isDocWrapperLine(trimmed) {
				continue
			}
			goto done

		case DocLangDashDash:
			if len(trimmed) == 0 {
				if len(collected) == 0 {
					continue
				}
				goto done
			}
			if bytes.HasPrefix(trimmed, dashDash) {
				collected = append(collected, stripDashCommentBytes(trimmed))
				continue
			}
			if len(collected) == 0 && isDocWrapperLine(trimmed) {
				continue
			}
			goto done

		case DocLangCSharpXML:
			if len(trimmed) == 0 {
				if len(collected) == 0 {
					continue
				}
				goto done
			}
			if bytes.HasPrefix(trimmed, tripleSlash) {
				collected = append(collected, stripCSharpXMLLineBytes(trimmed))
				continue
			}
			if len(collected) == 0 && isDocWrapperLine(trimmed) {
				continue
			}
			goto done
		}
	}

done:
	if len(collected) == 0 {
		return ""
	}
	// collected is in reverse order (we walked upward). Reverse.
	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	return firstParagraph(collected)
}

// ExtractPyDocstring extracts a Python docstring: the first string
// literal in the function/class body. bodyText is the raw source text
// of the suite/block node. Returns the first paragraph (text up to the
// first blank line), truncated to docMaxLen. Returns "" if no
// docstring.
func ExtractPyDocstring(bodyText string) string {
	s := strings.TrimLeft(bodyText, " \t\r\n")
	if s == "" {
		return ""
	}
	// Triple-quoted forms first.
	for _, q := range []string{`"""`, `'''`} {
		if strings.HasPrefix(s, q) {
			rest := s[3:]
			end := strings.Index(rest, q)
			if end < 0 {
				return ""
			}
			doc := strings.TrimSpace(rest[:end])
			return firstPyParagraph(doc)
		}
	}
	// Single-quoted single-line docstrings (rare but valid).
	for _, q := range []string{`"`, `'`} {
		if strings.HasPrefix(s, q) {
			rest := s[1:]
			end := strings.Index(rest, q)
			if end < 0 {
				return ""
			}
			line := strings.TrimSpace(rest[:end])
			if line == "" {
				return ""
			}
			return truncateDoc(line)
		}
	}
	return ""
}

// firstPyParagraph collapses the first paragraph of a Python docstring
// to a single line, truncated to docMaxLen. A "paragraph" is text up
// to the first blank-line gap.
func firstPyParagraph(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		ln := strings.TrimSpace(line)
		if ln == "" {
			if b.Len() > 0 {
				break
			}
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(ln)
		if b.Len() > docMaxLen {
			break
		}
	}
	return truncateDoc(b.String())
}

// stripLineCommentPrefixBytes strips a leading comment-opener prefix
// (// or #) plus optional Rust-style /// // ! decorations and a
// trailing space, returning the result as a freshly allocated string.
// Operates on []byte input so callers (ExtractDocAbove) can defer the
// string allocation until they've decided the line is actually part of
// the doc block.
func stripLineCommentPrefixBytes(line, prefix []byte) string {
	s := bytes.TrimPrefix(line, prefix)
	s = bytes.TrimPrefix(s, []byte("/"))
	s = bytes.TrimPrefix(s, []byte("!"))
	s = bytes.TrimPrefix(s, []byte(" "))
	return string(s)
}

// stripBlockStarLineBytes strips a leading "*" plus optional leading
// space from a continuation line inside a /** ... */ doc block. See
// stripLineCommentPrefixBytes for the alloc-deferral rationale.
func stripBlockStarLineBytes(line []byte) string {
	s := bytes.TrimSpace(line)
	s = bytes.TrimPrefix(s, []byte("*"))
	s = bytes.TrimPrefix(s, []byte(" "))
	return string(s)
}

// stripDashCommentBytes strips a leading "--" (Lua/SQL/Haskell/Ada line
// comment), the optional extra "-" of Lua's "---" doc form, and a trailing
// space, returning a freshly allocated string.
func stripDashCommentBytes(line []byte) string {
	s := bytes.TrimPrefix(line, []byte("--"))
	s = bytes.TrimPrefix(s, []byte("-"))
	s = bytes.TrimPrefix(s, []byte(" "))
	return string(s)
}

// stripCSharpXMLLineBytes strips leading "///" plus optional space
// from a C# XML doc line and drops any angle-bracketed XML tags,
// keeping only the inner text. Rough — the spec calls for <summary>
// contents only; we don't parse XML here, just remove tag wrappers.
func stripCSharpXMLLineBytes(line []byte) string {
	s := bytes.TrimPrefix(line, []byte("///"))
	s = bytes.TrimPrefix(s, []byte(" "))
	var b strings.Builder
	b.Grow(len(s))
	depth := 0
	// Ranging over string(s) decodes UTF-8 in place without copying
	// when s is the only reference — the compiler elides the alloc.
	for _, r := range string(s) {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// firstParagraph joins collected lines with a single space, stops at
// the first blank-line gap (already filtered) or a JSDoc/Javadoc
// `@param`/`@return`/etc. tag, and truncates to docMaxLen.
func firstParagraph(lines []string) string {
	var b strings.Builder
	for i, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			if b.Len() > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(ln, "@") {
			// JSDoc/Javadoc tag — first paragraph ended.
			break
		}
		if i > 0 && b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(ln)
		if b.Len() > docMaxLen {
			break
		}
	}
	return truncateDoc(b.String())
}

func truncateDoc(s string) string {
	if len(s) <= docMaxLen {
		return s
	}
	// Cut on a rune boundary.
	cut := docMaxLen
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// --- Visibility ----------------------------------------------------

// VisibilityPublic / VisibilityPrivate / etc. are the canonical values
// for Node.Meta["visibility"].
const (
	VisibilityPublic    = "public"
	VisibilityPrivate   = "private"
	VisibilityProtected = "protected"
	VisibilityInternal  = "internal"
	VisibilityPackage   = "package"
)

// VisibilityByCase returns "public" for Go-style identifiers (first
// rune uppercase ASCII) and "package" otherwise. Used by Go.
func VisibilityByCase(name string) string {
	if name == "" {
		return ""
	}
	c := name[0]
	if c >= 'A' && c <= 'Z' {
		return VisibilityPublic
	}
	return VisibilityPackage
}

// VisibilityByUnderscore returns "private" for names starting with "_"
// and "public" otherwise. Used by Python and Dart.
func VisibilityByUnderscore(name string) string {
	if name == "" {
		return ""
	}
	if name[0] == '_' {
		return VisibilityPrivate
	}
	return VisibilityPublic
}

// VisibilityFromModifiers picks the strongest known modifier from a
// list, with `defaultVis` as the fallback. Recognized modifiers:
// public, private, protected, internal, open (kotlin → public),
// fileprivate (swift → private), pub (rust → public),
// "pub(crate)" (rust → internal).
func VisibilityFromModifiers(modifiers []string, defaultVis string) string {
	for _, m := range modifiers {
		switch strings.TrimSpace(m) {
		case "public", "open", "pub":
			return VisibilityPublic
		case "private", "fileprivate":
			return VisibilityPrivate
		case "protected":
			return VisibilityProtected
		case "internal", "pub(crate)":
			return VisibilityInternal
		case "package", "package-private":
			return VisibilityPackage
		}
	}
	return defaultVis
}

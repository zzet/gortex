package languages

import "bytes"

// maskSQLDollarQuotedBodies returns src with the content of every
// dollar-quoted string (`$$ … $$`, `$tag$ … $tag$`) blanked out, the
// quote delimiters themselves kept. Every masked byte becomes a space
// except '\n', so the result has the same length and the same line
// layout as src: node byte offsets and line ranges taken from a tree
// parsed over the mask are valid against the original source.
//
// tree-sitter-sql only understands a slice of PL/pgSQL. A function body
// containing `PERFORM …`, `IF NOT FOUND`, `IF NOT EXISTS (…)` or
// `IF … IS DISTINCT FROM …` turns the whole CREATE FUNCTION into an
// ERROR node, and the declaration vanished from the index. For Postgres
// the body is just a string literal, so its content must not decide
// whether the header parses; with the body blanked it always does.
//
// Comments, single-quoted strings and quoted identifiers are skipped so
// a `$$` inside them is not mistaken for a quote opener. src is returned
// unchanged (no copy) when it holds no dollar-quoted string.
func maskSQLDollarQuotedBodies(src []byte) []byte {
	if bytes.IndexByte(src, '$') < 0 {
		return src
	}
	var out []byte
	n := len(src)
	for i := 0; i < n; {
		c := src[i]
		switch {
		case c == '-' && i+1 < n && src[i+1] == '-':
			i = skipSQLLineComment(src, i)
		case c == '/' && i+1 < n && src[i+1] == '*':
			i = skipSQLBlockComment(src, i)
		case c == '\'':
			escapes := i > 0 && (src[i-1] == 'E' || src[i-1] == 'e') && (i < 2 || !isSQLIdentByte(src[i-2]))
			i = skipSQLQuoted(src, i, '\'', escapes)
		case c == '"':
			i = skipSQLQuoted(src, i, '"', false)
		case c == '$' && (i == 0 || !isSQLIdentByte(src[i-1])):
			tagEnd := sqlDollarTagEnd(src, i)
			if tagEnd < 0 {
				i++
				continue
			}
			tag := src[i:tagEnd]
			rel := bytes.Index(src[tagEnd:], tag)
			if rel < 0 {
				// Unterminated: leave the rest for the parser as-is.
				i = n
				continue
			}
			bodyEnd := tagEnd + rel
			if out == nil {
				out = append([]byte(nil), src...)
			}
			for j := tagEnd; j < bodyEnd; j++ {
				if out[j] != '\n' {
					out[j] = ' '
				}
			}
			i = bodyEnd + len(tag)
		default:
			i++
		}
	}
	if out == nil {
		return src
	}
	return out
}

// sqlDollarTagEnd returns the index just past the dollar-quote opener
// starting at src[i] (`$$` or `$tag$`), or -1 when src[i] does not open
// one — e.g. a positional parameter such as `$1`.
func sqlDollarTagEnd(src []byte, i int) int {
	j := i + 1
	if j < len(src) && src[j] == '$' {
		return j + 1
	}
	if j >= len(src) || !isSQLTagStartByte(src[j]) {
		return -1
	}
	for j++; j < len(src); j++ {
		if src[j] == '$' {
			return j + 1
		}
		if !isSQLTagStartByte(src[j]) && (src[j] < '0' || src[j] > '9') {
			return -1
		}
	}
	return -1
}

func skipSQLLineComment(src []byte, i int) int {
	if k := bytes.IndexByte(src[i:], '\n'); k >= 0 {
		return i + k + 1
	}
	return len(src)
}

// skipSQLBlockComment skips a `/* … */` comment; Postgres nests them.
func skipSQLBlockComment(src []byte, i int) int {
	depth := 0
	for i < len(src) {
		switch {
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '*':
			depth++
			i += 2
		case src[i] == '*' && i+1 < len(src) && src[i+1] == '/':
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return len(src)
}

// skipSQLQuoted skips a quote-delimited token starting at src[i]. A
// doubled quote is an escaped quote; backslash escapes apply only to
// E'…' strings.
func skipSQLQuoted(src []byte, i int, quote byte, backslashEscapes bool) int {
	for i++; i < len(src); i++ {
		switch src[i] {
		case '\\':
			if backslashEscapes {
				i++
			}
		case quote:
			if i+1 < len(src) && src[i+1] == quote {
				i++
				continue
			}
			return i + 1
		}
	}
	return len(src)
}

func isSQLTagStartByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b >= 0x80
}

func isSQLIdentByte(b byte) bool {
	return isSQLTagStartByte(b) || (b >= '0' && b <= '9') || b == '$'
}

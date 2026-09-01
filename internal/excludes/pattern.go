package excludes

import (
	"regexp"
	"strings"
)

// go-gitignore compiles a pattern by string-substituting it into a Go
// regular expression, and it escapes exactly two characters on the way:
// "." and "?". Every other regexp metacharacter in the pattern text
// reaches the engine as an operator, even though gitignore reads it as
// ordinary text. The results range from wrong to catastrophic:
//
//   - "*$" (an Emacs autosave pattern that appears verbatim in pandas'
//     .gitignore) compiles to `^(|.*/)([^/]*)$(|/.*)$`. The "$" is an
//     end-of-text anchor, so the pattern matches every path in the repo
//     and the whole tree is excluded from indexing — silently (#624).
//   - "^*" matches everything for the same reason.
//   - "a|b" also matches "b"; "foo+" also matches "foo".
//   - An unbalanced "[" or "(" makes regexp.Compile fail. go-gitignore
//     discards the error and drops the line, so the ignore stops
//     applying at all — the mirror-image failure.
//
// literalizePattern rewrites a gitignore pattern so none of that can
// happen: characters git treats as literal text are escaped before the
// library ever sees them, and the two glob constructs the library gets
// wrong ("?" and bracket expressions) are lowered here instead.
//
// "*" and "." are deliberately left alone — go-gitignore's own handling
// of those (including "**" across path separators) is correct, and
// pre-escaping "." would collide with its `\.` substitution pass.

// regexOnlyMeta are the regexp metacharacters that carry no meaning in a
// gitignore pattern. Each is escaped so it matches itself.
const regexOnlyMeta = `$^+(){}|]`

// literalizePattern returns pattern with every regexp-only metacharacter
// escaped and every glob construct lowered to the equivalent the
// underlying regexp engine understands. The result is intended for
// go-gitignore's compiler, not for display: callers keep the original
// text for anything user-visible.
func literalizePattern(pattern string) string {
	if pattern == "" {
		return pattern
	}
	// A leading "!" is go-gitignore's negation marker, not pattern text.
	// Keep it outside the rewrite so the line still reads as a negation.
	neg := ""
	body := pattern
	if strings.HasPrefix(body, "!") {
		neg, body = "!", body[1:]
	}

	var b strings.Builder
	b.Grow(len(body) + 8)
	for i := 0; i < len(body); {
		c := body[i]
		switch {
		case c == '\\':
			// A gitignore backslash escape. Copy the pair through
			// untouched: go-gitignore maps `\*` back to a literal star,
			// and every other `\X` already reaches the regexp as a
			// literal X.
			if i+1 < len(body) {
				b.WriteByte('\\')
				b.WriteByte(body[i+1])
				i += 2
				continue
			}
			// A trailing lone backslash would be a dangling escape and
			// the pattern would not compile at all. Make it literal.
			b.WriteString(`\\`)
			i++
		case c == '[':
			if class, n, ok := scanBracketExpr(body[i:]); ok {
				b.WriteString(class)
				i += n
				continue
			}
			// An unterminated "[" is ordinary text to fnmatch, and so to
			// git. Left alone it would fail to compile and take the whole
			// pattern down with it.
			b.WriteString(`\[`)
			i++
		case c == '?':
			// gitignore: any single character except "/". go-gitignore
			// escapes it into a literal "?" instead, which under-matches.
			b.WriteString(`[^/]`)
			i++
		case strings.IndexByte(regexOnlyMeta, c) >= 0:
			b.WriteByte('\\')
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return neg + b.String()
}

// scanBracketExpr reads a bracket expression at the start of s (which
// must begin with "["). It returns the rewritten expression, how many
// bytes of s it consumed, and whether s opens a usable one at all.
//
// Two shapes need rewriting for the regexp engine: git's negation marker
// is "!" where regexp wants "^", and a "*" or "$" inside the brackets is
// literal text that go-gitignore's later substitution passes would
// otherwise rewrite (its "*" expansion, and its "#$~" sentinel). Anything
// that still fails to compile is reported as "not a bracket expression"
// so the caller falls back to treating the "[" as literal text.
func scanBracketExpr(s string) (string, int, bool) {
	j := 1 // s[0] == '['
	negated := false
	if j < len(s) && (s[j] == '!' || s[j] == '^') {
		negated = true
		j++
	}
	// A "]" in the first position is literal, per POSIX bracket rules.
	if j < len(s) && s[j] == ']' {
		j++
	}
	for j < len(s) {
		switch s[j] {
		case '\\':
			j += 2
			continue
		case '/':
			// gitignore wildcards never cross a path separator, so a "["
			// that only closes past one was never a bracket expression.
			return "", 0, false
		case ']':
			inner := s[1:j]
			if negated {
				inner = "^" + inner[1:]
			}
			out := "[" + escapeInBracketExpr(inner) + "]"
			if _, err := regexp.Compile(out); err != nil {
				return "", 0, false
			}
			return out, j + 1, true
		}
		j++
	}
	return "", 0, false
}

// escapeInBracketExpr escapes the two characters that are literal inside
// a bracket expression but that go-gitignore's post-processing would
// still rewrite: "*" (expanded to a capture group) and "$" (half of its
// "#$~" placeholder). Escaping them here survives that pass — the
// library maps `\*` back to a literal star, and `\$` never forms the
// placeholder.
func escapeInBracketExpr(inner string) string {
	if !strings.ContainsAny(inner, `*$`) {
		return inner
	}
	var b strings.Builder
	b.Grow(len(inner) + 4)
	for i := 0; i < len(inner); i++ {
		switch c := inner[i]; c {
		case '\\':
			b.WriteByte(c)
			if i+1 < len(inner) {
				i++
				b.WriteByte(inner[i])
			}
		case '*', '$':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

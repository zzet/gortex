package languages

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

var (
	// templateBlockRe matches <script>/<style> blocks, whose bodies are excluded
	// from the template scan (their content is handled by script delegation).
	templateBlockRe = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(?:script|style)>`)
	// templateTagRe captures an opening tag name: <Name, <my-name, <svelte:self.
	templateTagRe = regexp.MustCompile(`<([A-Za-z][\w.-]*(?::[\w.-]+)?)`)
)

// templateBuiltins are framework component names that are not real component
// files and must never become cross-file references.
var templateBuiltins = map[string]bool{
	"Teleport": true, "Suspense": true, "KeepAlive": true, "Transition": true,
	"TransitionGroup": true, "Component": true, "Slot": true, "Template": true,
	"Fragment": true, "Code": true, "Debug": true, "Comment": true,
	// Nuxt framework components — auto-registered, no component file.
	"NuxtPage": true, "NuxtLayout": true, "NuxtLink": true, "NuxtLoadingIndicator": true,
	"NuxtErrorBoundary": true, "NuxtWelcome": true, "NuxtClientFallback": true,
	"NuxtRouteAnnouncer": true, "NuxtImg": true, "NuxtPicture": true, "NuxtIsland": true,
	"ClientOnly": true, "DevOnly": true,
}

// blazorBuiltins are Blazor framework components that ship with ASP.NET Core
// and have no user component file — so a `<Router>` / `<EditForm>` /
// `<InputText>` markup tag must never become a dangling cross-file reference.
// Scoped to Razor only (via templateBuiltinsFor), because several of these
// names (EditForm, InputText, …) are plausible user component names in other
// template languages.
var blazorBuiltins = map[string]bool{
	"Router": true, "RouteView": true, "AuthorizeRouteView": true, "Found": true,
	"NotFound": true, "Navigating": true, "LayoutView": true, "NavLink": true,
	"CascadingValue": true, "CascadingAuthenticationState": true,
	"DynamicComponent": true, "ErrorBoundary": true, "FocusOnNavigate": true,
	"HeadContent": true, "HeadOutlet": true, "PageTitle": true,
	"SectionContent": true, "SectionOutlet": true, "Virtualize": true,
	"AntiforgeryToken": true, "FormMappingScope": true,
	// Forms.
	"EditForm": true, "DataAnnotationsValidator": true, "ObjectGraphDataAnnotationsValidator": true,
	"ValidationSummary": true, "ValidationMessage": true, "InputText": true,
	"InputTextArea": true, "InputNumber": true, "InputSelect": true,
	"InputCheckbox": true, "InputDate": true, "InputRadio": true,
	"InputRadioGroup": true, "InputFile": true,
	// Authorization.
	"AuthorizeView": true, "Authorized": true, "NotAuthorized": true, "Authorizing": true,
}

// razorBuiltins is the union of the shared template builtins and the Blazor
// framework components, precomputed so the Razor tag scan skips both.
var razorBuiltins = unionStringSets(templateBuiltins, blazorBuiltins)

// unionStringSets returns the set union of its arguments.
func unionStringSets(sets ...map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, s := range sets {
		for k := range s {
			out[k] = true
		}
	}
	return out
}

// templateBuiltinsFor returns the framework-component skip set for a template
// language — the shared set everywhere, plus the Blazor builtins for Razor.
func templateBuiltinsFor(lang string) map[string]bool {
	if lang == "razor" {
		return razorBuiltins
	}
	return templateBuiltins
}

// stripNuxtLazyPrefix removes the Nuxt `Lazy` auto-import prefix so a
// `<LazyBaseButton>` lazy-hydrated usage references the same `BaseButton`
// component as `<BaseButton>`. Only strips when `Lazy` is followed by an
// uppercase letter (a PascalCase component head), so a component genuinely
// named `Lazy` or `Lazyload` is left intact.
func stripNuxtLazyPrefix(name string) string {
	const pfx = "Lazy"
	if len(name) > len(pfx) && strings.HasPrefix(name, pfx) && unicode.IsUpper(rune(name[len(pfx)])) {
		return name[len(pfx):]
	}
	return name
}

// mineTemplateComponentUsages scans the template region (everything outside
// <script>/<style>) and emits a reference edge from componentID to each distinct
// component tag it uses — the cross-file "this component renders that one" link
// that makes a child component a resolved dependent. Kebab-case custom elements
// are normalized to PascalCase (my-button -> MyButton); plain HTML elements and
// framework special elements (svelte:, astro:) are skipped.
func mineTemplateComponentUsages(src []byte, filePath, componentID, lang string, result *parser.ExtractionResult) {
	builtins := templateBuiltinsFor(lang)
	tmpl := blankTemplateRegions(src, lang)
	for _, idx := range templateTagRe.FindAllSubmatchIndex(tmpl, -1) {
		// idx[0:2] spans the whole `<Tag` match; idx[2:4] is the captured name.
		raw := string(tmpl[idx[2]:idx[3]])
		if !isComponentTagName(raw) {
			continue
		}
		name := stripNuxtLazyPrefix(componentRefName(raw))
		if name == "" || builtins[name] {
			continue
		}
		// One positioned edge per render site — NOT deduplicated by name. The
		// tag's line comes from its byte offset (blanked <script>/<style>
		// blocks preserve newlines, so offsets still map to source lines), and
		// each edge carries Origin=OriginASTResolved plus Meta[template]=true so
		// find_usages reports every render location with a line number, an AST
		// provenance tier, and a template-vs-code role — where a name-deduped
		// single reference would collapse repeated renders into one position.
		line := 1 + strings.Count(string(tmpl[:idx[0]]), "\n")
		result.Edges = append(result.Edges, &graph.Edge{
			From: componentID, To: "unresolved::" + name,
			Kind: graph.EdgeReferences, FilePath: filePath,
			Line: line, Origin: graph.OriginASTResolved,
			Meta: map[string]any{"template": true, "template_name": componentRefName(raw)},
		})
	}
}

// isComponentTagName reports whether a tag name denotes a component (PascalCase,
// or a hyphenated custom element) rather than a plain HTML element or a framework
// special element.
func isComponentTagName(raw string) bool {
	if raw == "" || strings.HasPrefix(raw, "svelte:") || strings.HasPrefix(raw, "astro:") {
		return false
	}
	if unicode.IsUpper(rune(raw[0])) {
		return true
	}
	return strings.Contains(raw, "-")
}

// componentRefName normalizes a tag name to a component symbol name (kebab-case
// custom elements are PascalCased; PascalCase tags are used verbatim).
func componentRefName(raw string) string {
	if raw == "" {
		return ""
	}
	if unicode.IsUpper(rune(raw[0])) {
		return raw
	}
	var b strings.Builder
	for _, p := range strings.Split(raw, "-") {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	return b.String()
}

// templateCalleeRe captures an identifier in call position (`fn(`) — group 1 is
// the callee name.
var templateCalleeRe = regexp.MustCompile(`([A-Za-z_$][\w$]*)\s*\(`)

// templateExprKeywords are JS/TS keywords that can appear in call position
// inside a `{...}` group but are not user functions.
var templateExprKeywords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "return": true,
	"typeof": true, "instanceof": true, "new": true, "in": true, "of": true,
	"await": true, "yield": true, "void": true, "delete": true, "do": true,
	"else": true, "case": true, "try": true, "catch": true, "throw": true,
	"function": true, "let": true, "const": true, "var": true, "as": true,
}

// mustacheSpan is the content range [start, end) between a `{` and its matching
// `}` in blanked SFC markup.
type mustacheSpan struct{ start, end int }

// templateMustacheSpans returns every top-level `{...}` group's content span,
// tracking brace depth and skipping braces inside string / template literals
// (so `` `${x}` `` and `"{"` do not confuse the depth count). A group that
// opens on one line and closes many lines later is returned as a single span.
func templateMustacheSpans(b []byte) []mustacheSpan {
	var spans []mustacheSpan
	depth, spanStart := 0, -1
	var strCh byte
	for i := 0; i < len(b); i++ {
		c := b[i]
		if strCh != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == strCh {
				strCh = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			strCh = c
		case '{':
			if depth == 0 {
				spanStart = i + 1
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && spanStart >= 0 {
					spans = append(spans, mustacheSpan{spanStart, i})
					spanStart = -1
				}
			}
		}
	}
	return spans
}

// mineTemplateExpressionCalls scans every `{...}` mustache group in SFC markup
// (Svelte / Astro / Vue) for call-position identifiers and emits a speculative
// call edge from the component to each — so a helper invoked only from markup
// (`class={cn(active)}`, `{fmt(price)}`) is not flagged dead and is reachable
// via find_usages / get_callers. The scan is brace-depth aware, so a group that
// opens on one line and closes many lines later (`{posts.map((p) => (`) is
// captured in one span. <script>/<style> blocks — and, for Astro, the leading
// frontmatter — are blanked first so their code is not double-scanned. Method
// calls (`x.map(...)`) and JS keywords are skipped; framework runes/macros are
// dropped by the later suppressFrameworkIdents pass.
func mineTemplateExpressionCalls(src []byte, filePath, componentID, lang string, result *parser.ExtractionResult) {
	if componentID == "" {
		return
	}
	tmpl := blankTemplateRegions(src, lang)
	seen := map[string]bool{}
	for _, span := range templateMustacheSpans(tmpl) {
		body := tmpl[span.start:span.end]
		for _, m := range templateCalleeRe.FindAllSubmatchIndex(body, -1) {
			// Skip a method call (`x.map(`) — only the receiver-less free
			// function resolves to a script-defined helper.
			if m[2] > 0 && body[m[2]-1] == '.' {
				continue
			}
			name := string(body[m[2]:m[3]])
			if templateExprKeywords[name] {
				continue
			}
			line := 1 + strings.Count(string(tmpl[:span.start+m[2]]), "\n")
			key := name + "\x00" + strconv.Itoa(line)
			if seen[key] {
				continue
			}
			seen[key] = true
			result.Edges = append(result.Edges, &graph.Edge{
				From: componentID, To: "unresolved::" + name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
				Origin: graph.OriginTextMatched,
				Meta:   map[string]any{"template": true, "via": "template_expr"},
			})
		}
	}
}

// blankTemplateRegions returns src with every non-markup region blanked
// (newline-preserving, so line numbers are intact): `<script>` / `<style>`
// blocks for all SFC languages, plus the leading `--- … ---` frontmatter fence
// for Astro. Centralising the masking gives the component-tag, expression-call
// and event-handler scans the same exclusion discipline — so an Astro
// frontmatter generic (`Map<Widget>`) is never misread as a `<Widget>` tag and
// a frontmatter call is never double-counted against the delegated TS.
func blankTemplateRegions(src []byte, lang string) []byte {
	tmpl := templateBlockRe.ReplaceAllFunc(src, blankPreservingNewlines)
	if lang == "astro" {
		tmpl = astroFrontmatterRe.ReplaceAllFunc(tmpl, blankPreservingNewlines)
	}
	return tmpl
}

// blankPreservingNewlines returns a same-length copy of b with every byte except
// newlines replaced by spaces, so a regex replacement keeps line numbers intact.
func blankPreservingNewlines(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c == '\n' {
			out[i] = '\n'
		} else {
			out[i] = ' '
		}
	}
	return out
}

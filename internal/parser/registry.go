package parser

import (
	"path/filepath"
	"strings"
)

// Registry maps languages and file extensions to extractors.
type Registry struct {
	extractors map[string]Extractor // language name -> extractor
	extMap     map[string]string    // file extension (with dot) -> language name
	nameMap    map[string]string    // exact basename (e.g. "Makefile", "Dockerfile") -> language
	// stemMap maps a basename stem declared as "<Stem>.*" (e.g.
	// "Dockerfile.*") to its language. It matches the per-purpose suffix
	// convention — Dockerfile.perf, Dockerfile.ci, Makefile.am — that the
	// exact-basename map cannot express.
	stemMap map[string]string
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		extractors: make(map[string]Extractor),
		extMap:     make(map[string]string),
		nameMap:    make(map[string]string),
		stemMap:    make(map[string]string),
	}
}

// Register adds an extractor and maps its extensions. Each entry in
// Extensions() is classified as one of three forms:
//
//   - ".ext"     — an extension, matched against the file's last or
//     compound extension
//   - "Name"     — a full basename like "Makefile" or "CMakeLists.txt",
//     matched against the file's basename exactly
//   - "Name.*"   — a basename STEM, matched against a basename of the
//     form "Name.<suffix>" for any suffix that is not itself a
//     registered extension (Dockerfile.perf, but not Dockerfile.md)
func (r *Registry) Register(e Extractor) {
	lang := e.Language()
	r.extractors[lang] = e
	for _, s := range e.Extensions() {
		switch {
		case strings.HasPrefix(s, "."):
			r.extMap[s] = lang
		case strings.HasSuffix(s, ".*"):
			r.stemMap[strings.TrimSuffix(s, ".*")] = lang
		default:
			r.nameMap[s] = lang
		}
	}
}

// GetByLanguage returns the extractor for the given language name.
func (r *Registry) GetByLanguage(lang string) (Extractor, bool) {
	e, ok := r.extractors[lang]
	return e, ok
}

// GetByExtension returns the extractor for the given file extension.
func (r *Registry) GetByExtension(ext string) (Extractor, bool) {
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	lang, ok := r.extMap[ext]
	if !ok {
		return nil, false
	}
	return r.extractors[lang], true
}

// DetectLanguage determines the language for a file path using only
// its name — the extension / basename mapping, with no content probe.
// Equivalent to DetectLanguageContent with nil content.
func (r *Registry) DetectLanguage(filePath string) (string, bool) {
	return r.DetectLanguageContent(filePath, nil)
}

// DetectLanguageContent determines the language for a file, using its
// content (when supplied) to disambiguate. Resolution order:
//
//  1. exact basename (Makefile, CMakeLists.txt)
//  2. compound extension (.blade.php, .html.erb)
//  3. single extension (.go) — for an ambiguous extension (.h, .m) a
//     content probe refines C vs C++ vs Objective-C / MATLAB / etc.
//  4. basename stem (Dockerfile.perf → "Dockerfile.*"). Deliberately
//     BELOW the extension lookup: a registered suffix still wins, so
//     Dockerfile.md is markdown while Dockerfile.perf is a Dockerfile.
//  5. unknown extension — a `#!` shebang line, when present, maps the
//     interpreter to a language (e.g. a .cgi Perl script)
//
// content may be nil; detection then degrades to name-based mapping,
// so DetectLanguage and a content-free DetectLanguageContent agree.
func (r *Registry) DetectLanguageContent(filePath string, content []byte) (string, bool) {
	base := filepath.Base(filePath)
	if lang, ok := r.nameMap[base]; ok {
		return lang, true
	}
	if idx := strings.LastIndex(base, "."); idx > 0 {
		if prev := strings.LastIndex(base[:idx], "."); prev >= 0 {
			if lang, ok := r.extMap[base[prev:]]; ok {
				return lang, true
			}
		}
	}
	ext := filepath.Ext(filePath)
	if lang, ok := r.extMap[ext]; ok {
		// Ambiguous extensions (.h, .m) get a content probe; the
		// refined language is used only when it has an extractor.
		if refined, refok := sniffAmbiguous(filePath, ext, content); refok {
			if _, registered := r.extractors[refined]; registered {
				return refined, true
			}
		}
		return lang, true
	}
	// Unknown suffix on a known basename stem — `Dockerfile.perf`,
	// `Containerfile.build`, `Makefile.am`. Reached only after the
	// extension lookups failed, so a real extension keeps its language.
	if idx := strings.Index(base, "."); idx > 0 {
		if lang, ok := r.stemMap[base[:idx]]; ok {
			return lang, true
		}
	}
	// Unknown extension — fall back to a shebang interpreter probe.
	if lang, ok := sniffShebang(content); ok {
		if _, registered := r.extractors[lang]; registered {
			return lang, true
		}
	}
	return "", false
}

// SupportedLanguages returns all registered language names.
func (r *Registry) SupportedLanguages() []string {
	langs := make([]string, 0, len(r.extractors))
	for lang := range r.extractors {
		langs = append(langs, lang)
	}
	return langs
}

// AssetClasses maps each registered language whose extractor is an
// AssetExtractor to its AssetClass. Languages backed by ordinary code
// extractors are absent. The indexer builds this once before a walk so it
// can apply corpus-admission caps by language without a per-file interface
// assertion.
func (r *Registry) AssetClasses() map[string]AssetClass {
	out := make(map[string]AssetClass)
	for lang, ext := range r.extractors {
		if class := AssetClassOf(ext); class != "" {
			out[lang] = class
		}
	}
	return out
}

// AssetExtensions returns the set of file extensions (with dot, as the
// extMap keys) whose extractor is an AssetExtractor — content consumed as
// raw bytes by image / PDF / office / data extractors. Lets the content-
// transform pipeline keep its byte-rewriting built-ins (the UTF-16
// decoder) away from payloads that exist to be read binary (#846).
func (r *Registry) AssetExtensions() map[string]bool {
	out := make(map[string]bool)
	for lang, ext := range r.extractors {
		if AssetClassOf(ext) == "" {
			continue
		}
		for e, l := range r.extMap {
			if l == lang {
				out[e] = true
			}
		}
	}
	return out
}

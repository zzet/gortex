package indexer

import (
	"reflect"
	"testing"
)

// TestStaleLangsDetection proves the per-language extractor-staleness signal:
// only languages whose stored version is behind the current one are flagged
// (so the advisory names the exact languages to reindex — a scoped reindex
// rather than a full cold rebuild), a language the snapshot never recorded is
// never spuriously flagged, and an empty baseline reports nothing.
func TestStaleLangsDetection(t *testing.T) {
	t.Run("only_behind_langs", func(t *testing.T) {
		stored := map[string]int{"go": 1, "python": 2, "ruby": 1}
		current := map[string]int{"go": 2, "python": 2, "ruby": 1, "rust": 3}
		got := staleLangsBetween(stored, current)
		// go is behind (1<2); python/ruby are current; rust is absent from
		// stored (no baseline) so it is NOT flagged.
		if want := []string{"go"}; !reflect.DeepEqual(got, want) {
			t.Errorf("staleLangsBetween = %v, want %v", got, want)
		}
	})

	t.Run("sorted_multiple", func(t *testing.T) {
		stored := map[string]int{"typescript": 1, "go": 1, "python": 1}
		current := map[string]int{"typescript": 2, "go": 2, "python": 1}
		got := staleLangsBetween(stored, current)
		if want := []string{"go", "typescript"}; !reflect.DeepEqual(got, want) {
			t.Errorf("staleLangsBetween = %v, want %v (sorted)", got, want)
		}
	})

	t.Run("json_and_empty", func(t *testing.T) {
		// An empty / unparseable baseline reports nothing.
		if got := ExtractorVersionStaleLangs(""); got != nil {
			t.Errorf("empty baseline = %v, want nil", got)
		}
		if got := ExtractorVersionStaleLangs("not json"); got != nil {
			t.Errorf("bad json = %v, want nil", got)
		}
		// Against the live extractor versions, an unchanged baseline language
		// is not stale. Java is the exemplar because its extractor has never
		// been bumped — a language whose version this suite also asserts
		// would make the check tautological.
		if got := ExtractorVersionStaleLangs(`{"java":1}`); len(got) != 0 {
			t.Errorf("stored at current = %v, want empty", got)
		}
		if got := ExtractorVersionStaleLangs(`{"java":1,"php":1}`); !reflect.DeepEqual(got, []string{"php"}) {
			t.Errorf("stored PHP structural-edge version = %v, want [php]", got)
		}
		// A store extracted before the generic-call fix must re-extract:
		// until then, every call spelling explicit type arguments is missing
		// from its graph entirely, and no content change will trigger it.
		if got := ExtractorVersionStaleLangs(`{"go":1,"scala":1,"cpp":1,"swift":1}`); !reflect.DeepEqual(
			got, []string{"cpp", "go", "scala", "swift"}) {
			t.Errorf("stored pre-generic-call version = %v, want all four", got)
		}
		// A store extracted before the C# params-shape fix must re-extract
		// unchanged .cs, .razor, and .cshtml files. Without the bump, their
		// persisted graph keeps the old arity and parameter evidence.
		if got := ExtractorVersionStaleLangs(`{"csharp":10}`); !reflect.DeepEqual(got, []string{"csharp"}) {
			t.Errorf("stored pre-params C# version = %v, want [csharp]", got)
		}
		for _, path := range []string{"src/Handler.cs", "Views/Page.razor", "Views/Page.cshtml"} {
			if got := merkleSaltFor(path); got != "csharp@12" {
				t.Errorf("C# extractor salt for %s = %q, want csharp@12", path, got)
			}
		}
		if got := merkleSaltFor("src/Handler.php"); got != "php@2" {
			t.Errorf("PHP extractor salt = %q, want php@2", got)
		}
		if got := merkleSaltFor("include/widget.hxx"); got != "cpp@2" {
			t.Errorf("C++ extractor salt for .hxx = %q, want cpp@2", got)
		}
	})

	t.Run("lang_for_file", func(t *testing.T) {
		if got := ExtractorLangForFile("internal/auth/token.go"); got != "go" {
			t.Errorf("ExtractorLangForFile(.go) = %q, want go", got)
		}
		if got := ExtractorLangForFile("include/widget.hxx"); got != "cpp" {
			t.Errorf("ExtractorLangForFile(.hxx) = %q, want cpp", got)
		}
		if got := ExtractorLangForFile("README.zzz"); got != "" {
			t.Errorf("ExtractorLangForFile(unknown) = %q, want \"\"", got)
		}
	})
}

package indexer

import (
	"encoding/json"
	"reflect"
	"testing"
)

// snapshotJSON renders the shape a real previous release persisted — the FULL
// per-language snapshot extractorVersionsSnapshot() writes — with the given
// overrides applied and the named languages removed. A stored row is never
// partial in production (persistRepoIndexState marshals the whole map and
// persistExtractorVersion only adds to it), so a test that feeds a two-key map
// is testing a shape the field never produces.
func snapshotJSON(t *testing.T, overrides map[string]int, drop ...string) string {
	t.Helper()
	m := extractorVersionsSnapshot()
	for _, lang := range drop {
		delete(m, lang)
	}
	for lang, v := range overrides {
		m[lang] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	return string(b)
}

// TestStaleLangsDetection proves both extractor-version precision and the
// global post-extraction policy epoch: new or behind language versions restage
// only their language, while a missing or behind global epoch restages every
// real current language. An empty baseline remains fail-inert.
func TestStaleLangsDetection(t *testing.T) {
	t.Run("only_behind_langs", func(t *testing.T) {
		stored := map[string]int{"go": 1, "python": 2, "ruby": 1}
		current := map[string]int{"go": 2, "python": 2, "ruby": 1, "rust": 3, "lua": 1}
		got := staleLangsBetween(stored, current)
		// go is behind (1<2); python/ruby are current. rust is absent from
		// stored, which means the snapshot's binary tracked no version for
		// it — the implicit baseline 1 — so a current version of 3 IS a
		// bump and must be flagged. lua is absent too but sits at the
		// baseline itself, so there is nothing to re-extract.
		if want := []string{"go", "rust"}; !reflect.DeepEqual(got, want) {
			t.Errorf("staleLangsBetween = %v, want %v", got, want)
		}
	})

	t.Run("no_baseline_reports_nothing", func(t *testing.T) {
		// Absent provenance means "unknown", not "everything is behind".
		if got := staleLangsBetween(nil, extractorVersionsSnapshot()); got != nil {
			t.Errorf("nil baseline = %v, want nil", got)
		}
		if got := staleLangsBetween(map[string]int{}, extractorVersionsSnapshot()); got != nil {
			t.Errorf("empty baseline = %v, want nil", got)
		}
	})

	t.Run("retired_language_is_not_compared", func(t *testing.T) {
		// A language the salt map no longer tracks has no current
		// version to compare against, so it is dropped rather than
		// flagged forever.
		stored := map[string]int{"go": 3, "retired": 9}
		if got := staleLangsBetween(stored, map[string]int{"go": 3}); got != nil {
			t.Errorf("retired language = %v, want nil", got)
		}
	})

	t.Run("newly_tracked_language_is_stale", func(t *testing.T) {
		// A language whose extension mapping ships in the SAME change that
		// raises its version cannot appear in the previous release's
		// snapshot. Comparing only the keys the snapshot happens to carry
		// would leave every already-indexed repository on the old
		// extraction forever, because no content change re-triggers it.
		previous := extractorVersionsSnapshot()
		delete(previous, "julia")

		stale := staleLangsBetween(previous, extractorVersionsSnapshot())
		var found bool
		for _, lang := range stale {
			if lang == "julia" {
				found = true
			}
		}
		if !found {
			t.Errorf("staleLangsBetween = %v, want it to contain julia", stale)
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

	t.Run("post_extraction_policy_epoch", func(t *testing.T) {
		current := map[string]int{
			postExtractionPolicySnapshotKey: postExtractionPolicyVersion,
			"go":                            3,
			"java":                          1,
			"ruby":                          1,
		}
		cases := []struct {
			name   string
			stored map[string]int
			want   []string
		}{
			{
				name:   "legacy_snapshot_missing_epoch",
				stored: map[string]int{"go": 3, "java": 1, "ruby": 1},
				want:   []string{"go", "java", "ruby"},
			},
			{
				name: "epoch_behind",
				stored: map[string]int{
					postExtractionPolicySnapshotKey: postExtractionPolicyVersion - 1,
					"go":                            3,
					"java":                          1,
					"ruby":                          1,
				},
				want: []string{"go", "java", "ruby"},
			},
			{
				name: "epoch_current_keeps_per_language_precision",
				stored: map[string]int{
					postExtractionPolicySnapshotKey: postExtractionPolicyVersion,
					"go":                            2,
					"java":                          1,
					"ruby":                          1,
				},
				want: []string{"go"},
			},
			{
				name: "all_current",
				stored: map[string]int{
					postExtractionPolicySnapshotKey: postExtractionPolicyVersion,
					"go":                            3,
					"java":                          1,
					"ruby":                          1,
				},
				want: nil,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := staleLangsBetween(tc.stored, current)
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("staleLangsBetween = %v, want %v", got, tc.want)
				}
				for _, lang := range got {
					if lang == postExtractionPolicySnapshotKey {
						t.Fatalf("reserved policy key leaked as a language: %v", got)
					}
				}
			})
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
		// A snapshot written by THIS binary is current in every language.
		if got := ExtractorVersionStaleLangs(snapshotJSON(t, nil)); len(got) != 0 {
			t.Errorf("stored at current = %v, want empty", got)
		}
		// A language MISSING from the snapshot but sitting at the
		// baseline is not stale: only a version raised above the
		// baseline is a bump. Java is the exemplar because its extractor
		// has never been bumped, which keeps the check from being
		// tautological.
		if got := ExtractorVersionStaleLangs(snapshotJSON(t, nil, "java")); len(got) != 0 {
			t.Errorf("absent baseline language = %v, want empty", got)
		}
		if got := ExtractorVersionStaleLangs(
			snapshotJSON(t, map[string]int{"php": 1})); !reflect.DeepEqual(got, []string{"php"}) {
			t.Errorf("stored PHP structural-edge version = %v, want [php]", got)
		}
		// A store extracted before the generic-call fix must re-extract:
		// until then, every call spelling explicit type arguments is missing
		// from its graph entirely, and no content change will trigger it.
		if got := ExtractorVersionStaleLangs(snapshotJSON(t, map[string]int{
			"go": 1, "scala": 1, "cpp": 1, "swift": 1,
		})); !reflect.DeepEqual(got, []string{"cpp", "go", "scala", "swift"}) {
			t.Errorf("stored pre-generic-call version = %v, want all four", got)
		}
		// A store extracted before the C# params-shape fix must re-extract
		// unchanged .cs, .razor, and .cshtml files. Without the bump, their
		// persisted graph keeps the old arity and parameter evidence.
		if got := ExtractorVersionStaleLangs(
			snapshotJSON(t, map[string]int{"csharp": 10})); !reflect.DeepEqual(got, []string{"csharp"}) {
			t.Errorf("stored pre-params C# version = %v, want [csharp]", got)
		}
		// The Julia tree-sitter extractor shipped `.jl` into the salt map
		// for the first time, so a previous release's snapshot has no julia
		// key at all — the shape that must still restage.
		if got := ExtractorVersionStaleLangs(
			snapshotJSON(t, nil, "julia")); !reflect.DeepEqual(got, []string{"julia"}) {
			t.Errorf("previous-release snapshot without julia = %v, want [julia]", got)
		}
		// A store extracted by the previous Julia extractor version must
		// re-extract unchanged .jl files too: the callee decoder, macro
		// and export handling, containment edges, and docstring metadata
		// all changed what the graph records without any content change.
		if got := ExtractorVersionStaleLangs(
			snapshotJSON(t, map[string]int{"julia": 2})); !reflect.DeepEqual(got, []string{"julia"}) {
			t.Errorf("snapshot stored at the previous julia version = %v, want [julia]", got)
		}
		if got := extractorVersionsSnapshot()[postExtractionPolicySnapshotKey]; got != postExtractionPolicyVersion {
			t.Errorf("persisted policy epoch = %d, want %d", got, postExtractionPolicyVersion)
		}

		policySalt := "_post_extraction_policy@2"
		for _, path := range []string{"model.py", "Model.java", "model.rb", "model.ts", "schema.ex", "model.js"} {
			if got := merkleSaltFor(path); got != policySalt {
				t.Errorf("global policy salt for %s = %q, want %q", path, got, policySalt)
			}
		}
		if got, want := merkleSaltFor("model.go"), policySalt+"|go@3"; got != want {
			t.Errorf("Go extractor salt = %q, want %q", got, want)
		}

		for _, path := range []string{"src/Handler.cs", "Views/Page.razor", "Views/Page.cshtml"} {
			want := policySalt + "|csharp@19"
			if got := merkleSaltFor(path); got != want {
				t.Errorf("C# extractor salt for %s = %q, want %q", path, got, want)
			}
		}
		if got, want := merkleSaltFor("src/Handler.php"), policySalt+"|php@2"; got != want {
			t.Errorf("PHP extractor salt = %q, want %q", got, want)
		}
		if got, want := merkleSaltFor("include/widget.hxx"), policySalt+"|cpp@2"; got != want {
			t.Errorf("C++ extractor salt for .hxx = %q, want %q", got, want)
		}
		if got := merkleSaltFor("README.zzz"); got != "" {
			t.Errorf("unmapped extension salt = %q, want empty", got)
		}
		// The Merkle half of the Julia bump: without the .jl → julia
		// mapping the leaf salt stays empty and Merkle mode misses the
		// bump the same way the mtime path did.
		if got, want := merkleSaltFor("src/model.jl"), policySalt+"|julia@3"; got != want {
			t.Errorf("Julia extractor salt = %q, want %q", got, want)
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

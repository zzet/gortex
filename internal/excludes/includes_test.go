package excludes

import "testing"

func TestIncludeAnchoring(t *testing.T) {
	for _, tc := range []struct {
		name         string
		patterns     []string
		include, rel string
		want         bool
	}{
		{"glob_star_dir", []string{"gen1/"}, "gen*/", "gen1/a.go", true},
		{"glob_question_dir", []string{"gen1/"}, "gen?/", "gen1/", true},
		{"glob_recursive_root", []string{"generated/"}, "**/generated/", "generated/a.go", true},
		{"glob_recursive_nested", []string{"generated/"}, "**/generated/", "src/generated/a.go", true},
		{"glob_without_slash", []string{"out/"}, "out*", "out/a.go", true},
		{"glob_closed_ancestor", []string{"src/", "generated/"}, "**/generated/", "src/generated/a.go", false},
		{"glob_vendor_contents", Builtin, "vendor/**", "vendor/a.go", false},
		{"glob_build_contents", []string{"build/"}, "build/**", "build/a.go", false},
		{"glob_build_open", []string{"build/**"}, "build/**", "build/a.go", true},
		{"basename_dir_nested", []string{"*.go"}, "Pods/", "ios/Pods/x.go", true},
		{"basename_dir_root", []string{"*.go"}, "Pods/", "Pods/x.go", true},
		{"basename_dir_closed_ancestor", []string{"ios/"}, "Pods/", "ios/Pods/x.go", false},
		{"basename_dir_closed_self", []string{"Pods/"}, "Pods/", "ios/Pods/x.go", true},
		{"basename_dir_reopens_self", []string{"Pods/"}, "Pods/", "ios/Pods/", true},
		{"basename_dir_closed_both", []string{"ios/", "Pods/"}, "Pods/", "ios/Pods/x.go", false},
		{"basename_literal_reopens_self", []string{"Pods/"}, "Pods", "ios/Pods/x.go", true},
		{"basename_dir_not_file", []string{"Pods"}, "Pods/", "ios/Pods", false},
		{"basename_dir_does_not_open_unknown_ancestor", []string{"ios/"}, "Pods/", "ios/", false},
		{"inner_glob_root", []string{"*.go"}, "ios/*", "ios/x.go", true},
		{"inner_glob_nested", []string{"*.go"}, "ios/*", "other/ios/x.go", false},
		{"root_glob_root", []string{"*.proto"}, "/*.proto", "x.proto", true},
		{"root_glob_nested", []string{"*.proto"}, "/*.proto", "src/x.proto", false},
		{"root_literal_nested", []string{"*.go"}, "/Pods/", "ios/Pods/x.go", false},
		{"root_literal_open_ancestor", []string{"ios/"}, "/ios/Pods/", "ios/", true},
		{"inner_slash_open_ancestor", []string{"ios/"}, "ios/Pods/", "ios/", true},
		{"inner_slash_root_only", []string{"*.go"}, "ios/Pods/", "other/ios/Pods/x.go", false},
		{"vendor_literal", Builtin, "vendor/acme/lib/", "vendor/acme/lib/x.go", true},
		{"node_modules_literal", Builtin, "node_modules/@org/pkg/", "node_modules/@org/pkg/x.go", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewWithIncludes(tc.patterns, []string{tc.include})
			if got := m.IncludesRel(tc.rel); got != tc.want {
				t.Errorf("IncludesRel(%q), include %q = %v, want %v", tc.rel, tc.include, got, tc.want)
			}
		})
	}
}

func TestIncludeBasenameOrder(t *testing.T) {
	for _, includes := range [][]string{{"ios/", "Pods/"}, {"Pods/", "ios/"}} {
		m := NewWithIncludes([]string{"ios/", "Pods/"}, includes)
		if !m.IncludesRel("ios/Pods/x.go") {
			t.Errorf("included parent must open its contents regardless of include order: %v", includes)
		}
	}
}

func TestParentLimitedIncludesAnchoredOnly(t *testing.T) {
	m := NewWithIncludes(Builtin, []string{"vendor/acme/keep.go", "/src/"})
	if got := m.ParentLimitedIncludes(); len(got) != 0 {
		t.Fatalf("anchored literals have no parent-limited patterns: %v", got)
	}
	if !m.IncludesRel("vendor/acme/keep.go") {
		t.Fatal("literal include lost")
	}
}

func TestIncludeAnchoringLiteralOpensGlobParent(t *testing.T) {
	m := NewWithIncludes(Builtin, []string{"vendor/acme/lib/", "*.proto"})
	if !m.IncludesRel("vendor/x.proto") || m.MatchRel("vendor/x.proto") {
		t.Fatal("literal include opens vendor/ so the wildcard can include its .proto sibling")
	}
	if m.IncludesRel("vendor/x.go") || !m.MatchRel("vendor/x.go") {
		t.Fatal("opening an ancestor must not include unrelated siblings")
	}
}

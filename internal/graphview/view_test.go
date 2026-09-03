package graphview

import (
	"encoding/hex"
	"errors"
	"slices"
	"testing"
)

func commitLayer(id string, gen int64) LayerRef {
	return LayerRef{Kind: LayerCommit, LayerID: id, Generation: gen}
}

func bufferLayer(id, fingerprint string) LayerRef {
	return LayerRef{Kind: LayerBuffer, LayerID: id, BufferFingerprint: fingerprint}
}

func TestLayerKindValid(t *testing.T) {
	for _, k := range []LayerKind{LayerCommit, LayerDirty, LayerBuffer} {
		if !k.Valid() {
			t.Errorf("%q is not Valid", k)
		}
	}
	for _, k := range []LayerKind{"", "overlay", "COMMIT"} {
		if k.Valid() {
			t.Errorf("%q reported itself Valid", k)
		}
	}
}

func TestLayerRefValidate(t *testing.T) {
	tests := []struct {
		name     string
		layer    LayerRef
		wantCode string
	}{
		{"commit layer", commitLayer("c1", 7), ""},
		{"dirty layer", LayerRef{Kind: LayerDirty, LayerID: "d1", Generation: 3}, ""},
		{"buffer layer", bufferLayer("b1", "sess:sha"), ""},
		{"unknown kind", LayerRef{Kind: "overlay", LayerID: "x", Generation: 1}, CodeInvalidViewSelector},
		{"empty kind", LayerRef{LayerID: "x", Generation: 1}, CodeInvalidViewSelector},
		{"missing layer id", LayerRef{Kind: LayerCommit, Generation: 1}, CodeInvalidViewSelector},
		{"commit without generation", LayerRef{Kind: LayerCommit, LayerID: "c1"}, CodeInvalidViewSelector},
		{"commit with negative generation", LayerRef{Kind: LayerCommit, LayerID: "c1", Generation: -1}, CodeInvalidViewSelector},
		{"dirty without generation", LayerRef{Kind: LayerDirty, LayerID: "d1"}, CodeInvalidViewSelector},
		{"commit with buffer fingerprint", LayerRef{Kind: LayerCommit, LayerID: "c1", Generation: 1, BufferFingerprint: "f"}, CodeInvalidViewSelector},
		{"buffer with generation", LayerRef{Kind: LayerBuffer, LayerID: "b1", Generation: 1, BufferFingerprint: "f"}, CodeInvalidViewSelector},
		{"buffer without fingerprint", LayerRef{Kind: LayerBuffer, LayerID: "b1"}, CodeInvalidViewSelector},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.layer.Validate()
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if got := CodeOf(err); got != tc.wantCode {
				t.Fatalf("CodeOf(Validate()) = %q, want %q (err=%v)", got, tc.wantCode, err)
			}
		})
	}
}

func TestLayerRefEqual(t *testing.T) {
	a := commitLayer("c1", 7)
	if !a.Equal(commitLayer("c1", 7)) {
		t.Error("identical layers are not Equal")
	}
	if a.Equal(commitLayer("c1", 8)) {
		t.Error("layers differing in generation are Equal")
	}
	if a.Equal(LayerRef{Kind: LayerDirty, LayerID: "c1", Generation: 7}) {
		t.Error("layers differing in kind are Equal")
	}
	if bufferLayer("b", "f1").Equal(bufferLayer("b", "f2")) {
		t.Error("buffer layers differing in fingerprint are Equal")
	}
}

func TestNewRepoViewID(t *testing.T) {
	v, err := NewRepoViewID("gortex", "graph-1", 12, commitLayer("c1", 13), bufferLayer("b1", "sess:sha"))
	if err != nil {
		t.Fatalf("NewRepoViewID() = %v", err)
	}
	if v.RepoPrefix != "gortex" || v.BaseGraphID != "graph-1" || v.BaseGeneration != 12 {
		t.Fatalf("unexpected repo view %+v", v)
	}
	if len(v.Layers) != 2 {
		t.Fatalf("layers = %v", v.Layers)
	}

	// The constructor copies the layers, so a caller's later mutation cannot
	// change an identity that has already been fingerprinted.
	layers := []LayerRef{commitLayer("c1", 13)}
	v2, err := NewRepoViewID("gortex", "graph-1", 12, layers...)
	if err != nil {
		t.Fatalf("NewRepoViewID() = %v", err)
	}
	before := v2.Fingerprint()
	layers[0] = commitLayer("c2", 99)
	if v2.Fingerprint() != before {
		t.Error("mutating the caller's slice changed the repo view fingerprint")
	}
}

func TestNewRepoViewIDRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		graph  string
		gen    int64
		layers []LayerRef
	}{
		{"missing prefix", "", "graph-1", 1, nil},
		{"missing graph id", "gortex", "", 1, nil},
		{"zero generation", "gortex", "graph-1", 0, nil},
		{"negative generation", "gortex", "graph-1", -4, nil},
		{"invalid layer", "gortex", "graph-1", 1, []LayerRef{{Kind: LayerCommit, LayerID: "c"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := NewRepoViewID(tc.prefix, tc.graph, tc.gen, tc.layers...)
			if err == nil {
				t.Fatalf("NewRepoViewID() = %+v, want an error", v)
			}
			if got := CodeOf(err); got != CodeInvalidViewSelector {
				t.Fatalf("CodeOf() = %q, want %q", got, CodeInvalidViewSelector)
			}
			if !errors.Is(err, ErrInvalidViewSelector) {
				t.Error("error does not match the invalid_view_selector sentinel")
			}
			if !v.Equal(RepoViewID{}) {
				t.Errorf("failed constructor returned %+v, want the zero value", v)
			}
		})
	}
}

func TestRepoViewIDEqual(t *testing.T) {
	base := RepoViewID{RepoPrefix: "a", BaseGraphID: "g", BaseGeneration: 1, Layers: []LayerRef{commitLayer("c", 2)}}
	same := RepoViewID{RepoPrefix: "a", BaseGraphID: "g", BaseGeneration: 1, Layers: []LayerRef{commitLayer("c", 2)}}
	if !base.Equal(same) {
		t.Error("identical repo views are not Equal")
	}
	others := []RepoViewID{
		{RepoPrefix: "b", BaseGraphID: "g", BaseGeneration: 1, Layers: []LayerRef{commitLayer("c", 2)}},
		{RepoPrefix: "a", BaseGraphID: "h", BaseGeneration: 1, Layers: []LayerRef{commitLayer("c", 2)}},
		{RepoPrefix: "a", BaseGraphID: "g", BaseGeneration: 2, Layers: []LayerRef{commitLayer("c", 2)}},
		{RepoPrefix: "a", BaseGraphID: "g", BaseGeneration: 1},
		{RepoPrefix: "a", BaseGraphID: "g", BaseGeneration: 1, Layers: []LayerRef{commitLayer("c", 2), commitLayer("d", 3)}},
		{RepoPrefix: "a", BaseGraphID: "g", BaseGeneration: 1, Layers: []LayerRef{commitLayer("d", 2)}},
	}
	for i, other := range others {
		if base.Equal(other) {
			t.Errorf("case %d: differing repo views are Equal", i)
		}
	}
}

func TestRepoViewFingerprintIsDeterministic(t *testing.T) {
	v := RepoViewID{
		RepoPrefix:     "gortex",
		BaseGraphID:    "graph-1",
		BaseGeneration: 12,
		Layers:         []LayerRef{commitLayer("c1", 13), bufferLayer("b1", "sess:sha")},
	}
	first := v.Fingerprint()
	if len(first) != 64 {
		t.Fatalf("fingerprint %q is %d chars, want 64", first, len(first))
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Fatalf("fingerprint is not hex: %v", err)
	}
	for i := 0; i < 5; i++ {
		if got := v.Fingerprint(); got != first {
			t.Fatalf("fingerprint changed between calls: %q then %q", first, got)
		}
	}
	// A separately built but equal value fingerprints the same.
	clone := RepoViewID{
		RepoPrefix:     "gortex",
		BaseGraphID:    "graph-1",
		BaseGeneration: 12,
		Layers:         []LayerRef{commitLayer("c1", 13), bufferLayer("b1", "sess:sha")},
	}
	if clone.Fingerprint() != first {
		t.Error("equal repo views fingerprint differently")
	}
}

// TestFingerprintsAreStableAcrossBuilds pins the canonical encoding. A change
// to the encoding invalidates every fingerprint a client has cached, so it has
// to be a deliberate act — bumping the domain tag — not a silent side effect.
func TestFingerprintsAreStableAcrossBuilds(t *testing.T) {
	repo := RepoViewID{
		RepoPrefix:     "gortex",
		BaseGraphID:    "graph-1",
		BaseGeneration: 12,
		Layers:         []LayerRef{commitLayer("c1", 13), bufferLayer("b1", "sess:sha")},
	}
	const wantRepo = "9cf01efe0dc96f142e37a5c5ea068f5a4b2b1b37bb8afefcf8bcf8a281d06dbb"
	if got := repo.Fingerprint(); got != wantRepo {
		t.Errorf("RepoViewID.Fingerprint() = %q, want %q", got, wantRepo)
	}

	ws := WorkspaceViewID{Repos: []RepoViewID{repo}}
	const wantWorkspace = "851804b69e23404409204d1049b57881acf448d7a30394c1dc7635802a6ef04e"
	if got := ws.Fingerprint(); got != wantWorkspace {
		t.Errorf("WorkspaceViewID.Fingerprint() = %q, want %q", got, wantWorkspace)
	}
}

func TestRepoViewFingerprintSeesEveryField(t *testing.T) {
	base := RepoViewID{
		RepoPrefix:     "gortex",
		BaseGraphID:    "graph-1",
		BaseGeneration: 12,
		Layers:         []LayerRef{commitLayer("c1", 13), bufferLayer("b1", "sess:sha")},
	}
	variants := map[string]RepoViewID{
		"repo prefix":        {RepoPrefix: "gortex2", BaseGraphID: "graph-1", BaseGeneration: 12, Layers: base.Layers},
		"base graph id":      {RepoPrefix: "gortex", BaseGraphID: "graph-2", BaseGeneration: 12, Layers: base.Layers},
		"base generation":    {RepoPrefix: "gortex", BaseGraphID: "graph-1", BaseGeneration: 13, Layers: base.Layers},
		"layer kind":         {RepoPrefix: "gortex", BaseGraphID: "graph-1", BaseGeneration: 12, Layers: []LayerRef{{Kind: LayerDirty, LayerID: "c1", Generation: 13}, bufferLayer("b1", "sess:sha")}},
		"layer id":           {RepoPrefix: "gortex", BaseGraphID: "graph-1", BaseGeneration: 12, Layers: []LayerRef{commitLayer("c2", 13), bufferLayer("b1", "sess:sha")}},
		"layer generation":   {RepoPrefix: "gortex", BaseGraphID: "graph-1", BaseGeneration: 12, Layers: []LayerRef{commitLayer("c1", 14), bufferLayer("b1", "sess:sha")}},
		"buffer fingerprint": {RepoPrefix: "gortex", BaseGraphID: "graph-1", BaseGeneration: 12, Layers: []LayerRef{commitLayer("c1", 13), bufferLayer("b1", "sess:other")}},
		"layer order":        {RepoPrefix: "gortex", BaseGraphID: "graph-1", BaseGeneration: 12, Layers: []LayerRef{bufferLayer("b1", "sess:sha"), commitLayer("c1", 13)}},
		"dropped layer":      {RepoPrefix: "gortex", BaseGraphID: "graph-1", BaseGeneration: 12, Layers: []LayerRef{commitLayer("c1", 13)}},
		"no layers":          {RepoPrefix: "gortex", BaseGraphID: "graph-1", BaseGeneration: 12},
	}
	want := base.Fingerprint()
	for name, v := range variants {
		t.Run(name, func(t *testing.T) {
			if got := v.Fingerprint(); got == want {
				t.Errorf("changing the %s did not change the fingerprint", name)
			}
			if base.Equal(v) {
				t.Errorf("changing the %s left the views Equal", name)
			}
		})
	}
}

// TestFingerprintCanonicalizationDefeatsJoinCollisions crafts identity pairs
// that a naive encoding would map to the same string: joining the fields with a
// separator collides when the separator appears inside a field, and
// concatenating them without one collides whenever a field boundary can slide.
// Length-prefixing every field makes both impossible.
func TestFingerprintCanonicalizationDefeatsJoinCollisions(t *testing.T) {
	repoPairs := []struct {
		name string
		a, b RepoViewID
	}{
		{
			name: "separator inside a field",
			a:    RepoViewID{RepoPrefix: "a", BaseGraphID: "b|c", BaseGeneration: 1},
			b:    RepoViewID{RepoPrefix: "a|b", BaseGraphID: "c", BaseGeneration: 1},
		},
		{
			name: "sliding boundary without a separator",
			a:    RepoViewID{RepoPrefix: "ab", BaseGraphID: "c", BaseGeneration: 1},
			b:    RepoViewID{RepoPrefix: "a", BaseGraphID: "bc", BaseGeneration: 1},
		},
		{
			name: "sliding boundary inside a layer",
			a:    RepoViewID{RepoPrefix: "r", BaseGraphID: "g", BaseGeneration: 1, Layers: []LayerRef{bufferLayer("a", "bc")}},
			b:    RepoViewID{RepoPrefix: "r", BaseGraphID: "g", BaseGeneration: 1, Layers: []LayerRef{bufferLayer("ab", "c")}},
		},
		{
			name: "empty field versus absent field",
			a:    RepoViewID{RepoPrefix: "r", BaseGraphID: "", BaseGeneration: 1, Layers: []LayerRef{commitLayer("", 1)}},
			b:    RepoViewID{RepoPrefix: "r", BaseGraphID: "", BaseGeneration: 1},
		},
	}
	seen := map[string]string{}
	for _, tc := range repoPairs {
		t.Run(tc.name, func(t *testing.T) {
			fa, fb := tc.a.Fingerprint(), tc.b.Fingerprint()
			if fa == fb {
				t.Fatalf("%+v and %+v collide on %s", tc.a, tc.b, fa)
			}
			for _, f := range []string{fa, fb} {
				if prev, ok := seen[f]; ok {
					t.Fatalf("fingerprint %s reused by %q and %q", f, prev, tc.name)
				}
				seen[f] = tc.name
			}
		})
	}

	// One workspace of two repos versus one workspace of a single repo whose
	// graph id spells out the join of the other's fields.
	twoRepos := WorkspaceViewID{Repos: []RepoViewID{
		{RepoPrefix: "a", BaseGraphID: "b", BaseGeneration: 1},
		{RepoPrefix: "c", BaseGraphID: "d", BaseGeneration: 1},
	}}
	oneRepo := WorkspaceViewID{Repos: []RepoViewID{
		{RepoPrefix: "a", BaseGraphID: "b|c|d", BaseGeneration: 1},
	}}
	if twoRepos.Fingerprint() == oneRepo.Fingerprint() {
		t.Error("workspace repo boundaries collide")
	}
	if twoRepos.Equal(oneRepo) {
		t.Error("workspaces with different repo sets are Equal")
	}

	// An empty workspace must not collide with a workspace holding one empty
	// repo view: the repo count is encoded, not implied.
	empty := WorkspaceViewID{}
	oneEmpty := WorkspaceViewID{Repos: []RepoViewID{{}}}
	if empty.Fingerprint() == oneEmpty.Fingerprint() {
		t.Error("an empty workspace collides with a workspace of one empty repo")
	}

	// A repo view and a workspace view holding only that repo are different
	// things, so the domain tags must keep their encodings apart.
	solo := RepoViewID{RepoPrefix: "a", BaseGraphID: "b", BaseGeneration: 1}
	if solo.Fingerprint() == (WorkspaceViewID{Repos: []RepoViewID{solo}}).Fingerprint() {
		t.Error("a repo view collides with the workspace that holds only it")
	}
}

func TestNewWorkspaceViewIDSortsRepos(t *testing.T) {
	a, err := NewRepoViewID("a-repo", "ga", 1)
	if err != nil {
		t.Fatalf("NewRepoViewID: %v", err)
	}
	b, err := NewRepoViewID("b-repo", "gb", 2, commitLayer("c", 5))
	if err != nil {
		t.Fatalf("NewRepoViewID: %v", err)
	}
	c, err := NewRepoViewID("c-repo", "gc", 3)
	if err != nil {
		t.Fatalf("NewRepoViewID: %v", err)
	}

	ws, err := NewWorkspaceViewID(c, a, b)
	if err != nil {
		t.Fatalf("NewWorkspaceViewID() = %v", err)
	}
	gotPrefixes := make([]string, 0, len(ws.Repos))
	for _, r := range ws.Repos {
		gotPrefixes = append(gotPrefixes, r.RepoPrefix)
	}
	if !slices.Equal(gotPrefixes, []string{"a-repo", "b-repo", "c-repo"}) {
		t.Fatalf("repos = %v, want canonical order", gotPrefixes)
	}
	if err := ws.Validate(); err != nil {
		t.Fatalf("constructed workspace does not validate: %v", err)
	}

	// The input order must not reach the fingerprint.
	other, err := NewWorkspaceViewID(b, c, a)
	if err != nil {
		t.Fatalf("NewWorkspaceViewID() = %v", err)
	}
	if ws.Fingerprint() != other.Fingerprint() {
		t.Error("workspace fingerprint depends on the order repos were passed in")
	}
	if !ws.Equal(other) {
		t.Error("workspaces built from the same repos are not Equal")
	}
	// A hand-assembled value in a non-canonical order fingerprints the same.
	handmade := WorkspaceViewID{Repos: []RepoViewID{c, b, a}}
	if handmade.Fingerprint() != ws.Fingerprint() {
		t.Error("Fingerprint does not canonicalize repo order")
	}
	if !handmade.Equal(ws) {
		t.Error("Equal does not canonicalize repo order")
	}
}

func TestNewWorkspaceViewIDRejects(t *testing.T) {
	valid, err := NewRepoViewID("a-repo", "ga", 1)
	if err != nil {
		t.Fatalf("NewRepoViewID: %v", err)
	}
	dup := valid
	dup.BaseGraphID = "gb"

	if _, err := NewWorkspaceViewID(valid, dup); err == nil {
		t.Fatal("two views of one repository were accepted")
	} else if got := CodeOf(err); got != CodeSelectorConflict {
		t.Errorf("CodeOf() = %q, want %q", got, CodeSelectorConflict)
	} else if !errors.Is(err, ErrSelectorConflict) {
		t.Error("error does not match the selector_conflict sentinel")
	}

	if _, err := NewWorkspaceViewID(valid, RepoViewID{RepoPrefix: "z-repo"}); err == nil {
		t.Fatal("an invalid repo view was accepted")
	} else if got := CodeOf(err); got != CodeInvalidViewSelector {
		t.Errorf("CodeOf() = %q, want %q", got, CodeInvalidViewSelector)
	}

	// The empty workspace is legal: a request can be in scope of no repository.
	ws, err := NewWorkspaceViewID()
	if err != nil {
		t.Fatalf("NewWorkspaceViewID() = %v", err)
	}
	if len(ws.Repos) != 0 {
		t.Errorf("repos = %v, want none", ws.Repos)
	}
	if err := ws.Validate(); err != nil {
		t.Errorf("empty workspace does not validate: %v", err)
	}
}

func TestWorkspaceValidateRejectsNonCanonicalOrder(t *testing.T) {
	a := RepoViewID{RepoPrefix: "a-repo", BaseGraphID: "ga", BaseGeneration: 1}
	b := RepoViewID{RepoPrefix: "b-repo", BaseGraphID: "gb", BaseGeneration: 1}
	err := WorkspaceViewID{Repos: []RepoViewID{b, a}}.Validate()
	if err == nil {
		t.Fatal("Validate() accepted repos out of canonical order")
	}
	if got := CodeOf(err); got != CodeInvalidViewSelector {
		t.Errorf("CodeOf() = %q, want %q", got, CodeInvalidViewSelector)
	}
}

// TestViewFileURI pins the identity a file location carries under a pinned
// view. It stands in for an absolute path when the content the view serves
// exists nowhere on disk, so it has to name the exact content and survive any
// path a repository can contain.
func TestViewFileURI(t *testing.T) {
	tests := []struct {
		name        string
		fingerprint string
		repoPrefix  string
		relPath     string
		want        string
	}{
		{"plain", "abc123", "repo", "internal/foo.go", "gortex-view://abc123/repo/internal/foo.go"},
		{"no prefix", "abc123", "", "foo.go", "gortex-view://abc123/foo.go"},
		{"space in a segment", "abc123", "repo", "a b/c.go", "gortex-view://abc123/repo/a%20b/c.go"},
		{"hash in a segment", "abc123", "repo", "a#b.go", "gortex-view://abc123/repo/a%23b.go"},
		{"question mark", "abc123", "repo", "a?b.go", "gortex-view://abc123/repo/a%3Fb.go"},
		{"non-ascii", "abc123", "repo", "документ.go",
			"gortex-view://abc123/repo/%D0%B4%D0%BE%D0%BA%D1%83%D0%BC%D0%B5%D0%BD%D1%82.go"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ViewFileURI(tc.fingerprint, tc.repoPrefix, tc.relPath); got != tc.want {
				t.Fatalf("ViewFileURI() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestViewFileURIDistinguishesViews pins that the identity is a function of
// the content: two views of the same path fingerprint differently, so a URI
// from one never resolves against the other.
func TestViewFileURIDistinguishesViews(t *testing.T) {
	a, err := NewRepoViewID("repo", "graph-1", 7)
	if err != nil {
		t.Fatalf("NewRepoViewID: %v", err)
	}
	b, err := NewRepoViewID("repo", "graph-1", 8)
	if err != nil {
		t.Fatalf("NewRepoViewID: %v", err)
	}
	if ViewFileURI(a.Fingerprint(), "repo", "foo.go") == ViewFileURI(b.Fingerprint(), "repo", "foo.go") {
		t.Fatal("two views of one path produced one URI")
	}
}

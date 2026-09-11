package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graphview"
)

// dependencyRevisionCohort is the shared fixture: a two-repository cohort with
// one cross-repository input, one ownership fact, a named configuration section
// per non-index domain, a producer policy and a capability set.
func dependencyRevisionCohort() DependencyRevisionInputs {
	return DependencyRevisionInputs{
		Target:         DependencyRevisionTarget{RepoPrefix: "alpha", WorkspaceID: "ws", ProjectID: "pj"},
		RosterComplete: true,
		Repositories: []DependencyRevisionRepository{
			{RepoPrefix: "alpha", Kind: DependencyRevisionRepositoryDedicated,
				GraphID: "g-a", CheckoutID: "co-a", Incarnation: "inc-a", SourceIdentity: "src-a"},
			{RepoPrefix: "beta", Kind: DependencyRevisionRepositoryRaw,
				Incarnation: "inc-b", RootIdentity: "root-b", SourceIdentity: "src-b"},
		},
		OwnershipComplete: true,
		Ownership: []DependencyRevisionOwnership{
			{RepoPrefix: "alpha", Language: "go", Scope: "internal/x", Owner: "example.com/alpha"},
			{RepoPrefix: "beta", Language: "go", Scope: "", Owner: "example.com/beta"},
		},
		Config: config.IndexConfig{},
		ConfigSections: []DependencyRevisionConfigSection{
			{Name: DependencyRevisionConfigLSP, Digest: "lsp-digest"},
			{Name: DependencyRevisionConfigArtifacts, Digest: "art-digest"},
			{Name: DependencyRevisionConfigWorkspace, Digest: "workspace-digest"},
			{Name: DependencyRevisionConfigProject, Digest: "project-digest"},
			{Name: DependencyRevisionConfigSemantic, Digest: "semantic-digest"},
		},
		Producers: []DependencyRevisionProducer{
			{Producer: string(graphview.CapSearchText), State: "unavailable", Reason: "no working copy"},
			{Producer: string(graphview.CapSyntaxGraph), State: "complete"},
		},
		Capabilities: []string{
			string(graphview.CapSearchText),
			string(graphview.CapSyntaxGraph),
		},
		ExtractorVersions: `{"go":3}`,
	}
}

func mustComputeDependencyRevision(t *testing.T, inputs DependencyRevisionInputs) string {
	t.Helper()
	revision, err := ComputeDependencyRevision(inputs)
	if err != nil {
		t.Fatalf("ComputeDependencyRevision: %v", err)
	}
	if !strings.HasPrefix(revision, DependencyRevisionEncodingVersion+":") {
		t.Fatalf("revision %q does not carry the encoding version", revision)
	}
	return revision
}

// TestComputeDependencyRevisionDeterministicAndOrderInsensitive pins both
// halves of the producer's contract: the same cohort always digests to the same
// revision, and the order a caller happened to enumerate the roster, the
// ownership facts, the configuration sections, the producer policy or the
// capability set in is not part of the cohort.
func TestComputeDependencyRevisionDeterministicAndOrderInsensitive(t *testing.T) {
	want := mustComputeDependencyRevision(t, dependencyRevisionCohort())
	for range 8 {
		if got := mustComputeDependencyRevision(t, dependencyRevisionCohort()); got != want {
			t.Fatalf("revision is not deterministic: %q != %q", got, want)
		}
	}

	rng := rand.New(rand.NewSource(1))
	for range 32 {
		shuffled := dependencyRevisionCohort()
		rng.Shuffle(len(shuffled.Repositories), func(i, j int) {
			shuffled.Repositories[i], shuffled.Repositories[j] = shuffled.Repositories[j], shuffled.Repositories[i]
		})
		rng.Shuffle(len(shuffled.Ownership), func(i, j int) {
			shuffled.Ownership[i], shuffled.Ownership[j] = shuffled.Ownership[j], shuffled.Ownership[i]
		})
		rng.Shuffle(len(shuffled.ConfigSections), func(i, j int) {
			shuffled.ConfigSections[i], shuffled.ConfigSections[j] = shuffled.ConfigSections[j], shuffled.ConfigSections[i]
		})
		rng.Shuffle(len(shuffled.Producers), func(i, j int) {
			shuffled.Producers[i], shuffled.Producers[j] = shuffled.Producers[j], shuffled.Producers[i]
		})
		rng.Shuffle(len(shuffled.Capabilities), func(i, j int) {
			shuffled.Capabilities[i], shuffled.Capabilities[j] = shuffled.Capabilities[j], shuffled.Capabilities[i]
		})
		if got := mustComputeDependencyRevision(t, shuffled); got != want {
			t.Fatalf("enumeration order entered the revision: %q != %q", got, want)
		}
	}

	// A repeated capability is a set member, not a second member.
	duplicated := dependencyRevisionCohort()
	duplicated.Capabilities = append(duplicated.Capabilities, string(graphview.CapSyntaxGraph))
	if got := mustComputeDependencyRevision(t, duplicated); got != want {
		t.Fatalf("a duplicate capability changed the revision: %q != %q", got, want)
	}

	// The caller's slices are inputs, not scratch space. Checked in both the
	// fixture's order and its reverse: for every one of the five lists at least
	// one of the two orders is non-canonical, so an anti-aliasing clone that
	// went missing has an in-place sort to show for it. Checking only the
	// fixture order would leave the roster and ownership clones unpinned —
	// those two fixture lists are already sorted, so their in-place sort is a
	// no-op and the assertion would pass with the clone deleted.
	//
	// This is not hypothetical for the roster: DependencyRevisionRoster
	// concatenates DedicatedOwners() and RawRegistrations(), each prefix-sorted
	// on its own but not against the other, so a dedicated "zeta" ahead of a
	// raw "alpha" is exactly the non-canonical order the wiring item will hand
	// in.
	for _, order := range []string{"fixture order", "reverse order"} {
		inputs := dependencyRevisionCohort()
		if order == "reverse order" {
			slices.Reverse(inputs.Repositories)
			slices.Reverse(inputs.Ownership)
			slices.Reverse(inputs.ConfigSections)
			slices.Reverse(inputs.Producers)
			slices.Reverse(inputs.Capabilities)
		}
		original := DependencyRevisionInputs{
			Repositories:   slices.Clone(inputs.Repositories),
			Ownership:      slices.Clone(inputs.Ownership),
			ConfigSections: slices.Clone(inputs.ConfigSections),
			Producers:      slices.Clone(inputs.Producers),
			Capabilities:   slices.Clone(inputs.Capabilities),
		}
		if got := mustComputeDependencyRevision(t, inputs); got != want {
			t.Fatalf("%s: enumeration order entered the revision: %q != %q", order, got, want)
		}
		switch {
		case !slices.Equal(inputs.Repositories, original.Repositories):
			t.Fatalf("%s: ComputeDependencyRevision reordered the caller's roster: %v", order, inputs.Repositories)
		case !slices.Equal(inputs.Ownership, original.Ownership):
			t.Fatalf("%s: ComputeDependencyRevision reordered the caller's ownership facts: %v", order, inputs.Ownership)
		case !slices.Equal(inputs.ConfigSections, original.ConfigSections):
			t.Fatalf("%s: ComputeDependencyRevision reordered the caller's config sections: %v", order, inputs.ConfigSections)
		case !slices.Equal(inputs.Producers, original.Producers):
			t.Fatalf("%s: ComputeDependencyRevision reordered the caller's producer policy: %v", order, inputs.Producers)
		case !slices.Equal(inputs.Capabilities, original.Capabilities):
			t.Fatalf("%s: ComputeDependencyRevision reordered the caller's capabilities: %v", order, inputs.Capabilities)
		}
	}
}

// TestComputeDependencyRevisionSensitiveToEveryCohortDimension mutates one
// cohort member at a time and requires the revision to move. Every dimension
// the handoff names as resolver-visible is represented: roster membership and
// identity, cross-repository source identity, ownership, the deep index
// configuration, the named configuration domains, producer policy, the
// capability set and extractor versions.
func TestComputeDependencyRevisionSensitiveToEveryCohortDimension(t *testing.T) {
	base := mustComputeDependencyRevision(t, dependencyRevisionCohort())
	cases := []struct {
		name  string
		apply func(*DependencyRevisionInputs)
	}{
		{"target repository", func(in *DependencyRevisionInputs) {
			in.Target.RepoPrefix = "beta"
		}},
		{"target workspace", func(in *DependencyRevisionInputs) {
			in.Target.WorkspaceID = "other-ws"
		}},
		{"target project", func(in *DependencyRevisionInputs) {
			in.Target.ProjectID = "other-pj"
		}},
		{"roster gains a cross-repository input", func(in *DependencyRevisionInputs) {
			in.Repositories = append(in.Repositories, DependencyRevisionRepository{
				RepoPrefix: "gamma", Kind: DependencyRevisionRepositoryRaw,
				Incarnation: "inc-c", RootIdentity: "root-c", SourceIdentity: "src-c"})
		}},
		{"roster loses a cross-repository input", func(in *DependencyRevisionInputs) {
			in.Repositories = in.Repositories[:1]
			in.Ownership = in.Ownership[:1]
		}},
		{"repository incarnation", func(in *DependencyRevisionInputs) {
			in.Repositories[1].Incarnation = "inc-b2"
		}},
		{"repository graph identity", func(in *DependencyRevisionInputs) {
			in.Repositories[0].GraphID = "g-a2"
		}},
		{"repository checkout identity", func(in *DependencyRevisionInputs) {
			in.Repositories[0].CheckoutID = "co-a2"
		}},
		{"repository root identity", func(in *DependencyRevisionInputs) {
			in.Repositories[1].RootIdentity = "root-b2"
		}},
		{"target source identity", func(in *DependencyRevisionInputs) {
			in.Repositories[0].SourceIdentity = "src-a2"
		}},
		{"cross-repository source identity", func(in *DependencyRevisionInputs) {
			in.Repositories[1].SourceIdentity = "src-b2"
		}},
		{"repository kind", func(in *DependencyRevisionInputs) {
			in.Repositories[1] = DependencyRevisionRepository{
				RepoPrefix: "beta", Kind: DependencyRevisionRepositoryDedicated,
				GraphID: "g-b", CheckoutID: "co-b", Incarnation: "inc-b", SourceIdentity: "src-b"}
		}},
		{"ownership owner", func(in *DependencyRevisionInputs) {
			in.Ownership[0].Owner = "example.com/alpha/v2"
		}},
		{"ownership scope", func(in *DependencyRevisionInputs) {
			in.Ownership[0].Scope = "internal/y"
		}},
		{"ownership language", func(in *DependencyRevisionInputs) {
			in.Ownership[0].Language = "rust"
		}},
		{"ownership gains a fact", func(in *DependencyRevisionInputs) {
			in.Ownership = append(in.Ownership, DependencyRevisionOwnership{
				RepoPrefix: "alpha", Language: "go", Scope: "internal/z", Owner: "example.com/alpha"})
		}},
		{"ownership loses a fact", func(in *DependencyRevisionInputs) {
			in.Ownership = in.Ownership[:1]
		}},
		{"index configuration gains a framework synthesizer", func(in *DependencyRevisionInputs) {
			synthesizers := []string{"gin"}
			in.Config.FrameworkSynthesizers = &synthesizers
		}},
		{"index configuration language set", func(in *DependencyRevisionInputs) {
			in.Config.Languages = []string{"go"}
		}},
		{"named configuration digest", func(in *DependencyRevisionInputs) {
			in.ConfigSections[0].Digest = "lsp-digest-2"
		}},
		// Losing a domain is not here because it is stronger than a moved
		// revision: every required domain's absence is a refusal, pinned one
		// domain at a time by
		// TestComputeDependencyRevisionRequiresEveryConfigurationDomain.
		{"named configuration gains a domain beyond the required set", func(in *DependencyRevisionInputs) {
			in.ConfigSections = append(in.ConfigSections,
				DependencyRevisionConfigSection{Name: "vector", Digest: "vec-digest"})
		}},
		{"producer state", func(in *DependencyRevisionInputs) {
			in.Producers[0].State = "complete"
		}},
		{"producer reason", func(in *DependencyRevisionInputs) {
			in.Producers[0].Reason = "a committed tree has no working copy"
		}},
		{"producer policy gains a row", func(in *DependencyRevisionInputs) {
			in.Producers = append(in.Producers, DependencyRevisionProducer{
				Producer: string(graphview.CapSimilarity), State: "disabled_by_config"})
		}},
		{"capability set", func(in *DependencyRevisionInputs) {
			in.Capabilities = append(in.Capabilities, string(graphview.CapSearchVector))
		}},
		{"extractor versions", func(in *DependencyRevisionInputs) {
			in.ExtractorVersions = `{"go":4}`
		}},
	}
	seen := map[string]string{base: "unmutated cohort"}
	for _, tc := range cases {
		inputs := dependencyRevisionCohort()
		tc.apply(&inputs)
		got := mustComputeDependencyRevision(t, inputs)
		if got == base {
			t.Errorf("%s: revision did not change", tc.name)
			continue
		}
		if other, collided := seen[got]; collided {
			t.Errorf("%s: collides with %s", tc.name, other)
			continue
		}
		seen[got] = tc.name
	}
}

// dependencyRevisionGoldenPreimage pins the canonical encoding byte for byte.
//
// Only the index-configuration digest floats, because it is produced by
// snapshotDedicatedBaseConfig over config.IndexConfig — a struct other lanes
// legitimately extend, and whose own domain/version is pinned by that
// function's tests. Everything the encoding itself decides — the label
// vocabulary, the length delimiters, the NUL terminators, the field order, the
// list headers and the sort order — is literal here, so a serializer change
// fails this test rather than silently re-keying every stored generation.
const dependencyRevisionGoldenPreimage = "encoding:9:cohort-v1\x00" +
	"target.repo:5:alpha\x00" +
	"target.workspace:2:ws\x00" +
	"target.project:2:pj\x00" +
	"roster.scope:0:\x00" +
	"roster.count:1:2\x00" +
	"repo.prefix:5:alpha\x00repo.kind:9:dedicated\x00repo.graph:3:g-a\x00" +
	"repo.checkout:4:co-a\x00repo.incarnation:5:inc-a\x00repo.root:0:\x00repo.source:5:src-a\x00" +
	"repo.prefix:4:beta\x00repo.kind:3:raw\x00repo.graph:0:\x00" +
	"repo.checkout:0:\x00repo.incarnation:5:inc-b\x00repo.root:6:root-b\x00repo.source:5:src-b\x00" +
	"ownership.count:1:2\x00" +
	"own.repo:5:alpha\x00own.language:2:go\x00own.scope:10:internal/x\x00own.owner:17:example.com/alpha\x00" +
	"own.repo:4:beta\x00own.language:2:go\x00own.scope:0:\x00own.owner:16:example.com/beta\x00" +
	"config.index:64:%s\x00" +
	"config.sections.count:1:5\x00" +
	"config.section.name:9:artifacts\x00config.section.digest:10:art-digest\x00" +
	"config.section.name:3:lsp\x00config.section.digest:10:lsp-digest\x00" +
	"config.section.name:7:project\x00config.section.digest:14:project-digest\x00" +
	"config.section.name:8:semantic\x00config.section.digest:15:semantic-digest\x00" +
	"config.section.name:9:workspace\x00config.section.digest:16:workspace-digest\x00" +
	"producers.count:1:2\x00" +
	"producer.id:12:graph.syntax\x00producer.state:8:complete\x00producer.reason:0:\x00" +
	"producer.id:11:search.text\x00producer.state:11:unavailable\x00producer.reason:15:no working copy\x00" +
	"capabilities.count:1:2\x00" +
	"capability:12:graph.syntax\x00" +
	"capability:11:search.text\x00" +
	"extractors:8:{\"go\":3}\x00"

// TestComputeDependencyRevisionGoldenVector pins the encoding and the digest
// that is taken over it, so an accidental change to either is a test failure
// rather than a silent global cache invalidation.
func TestComputeDependencyRevisionGoldenVector(t *testing.T) {
	inputs := dependencyRevisionCohort()
	_, configDigest, err := snapshotDedicatedBaseConfig(
		inputs.Config, inputs.Target.RepoPrefix, inputs.Target.WorkspaceID, inputs.Target.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(configDigest) != 64 {
		t.Fatalf("index configuration digest is %d hex characters, want 64", len(configDigest))
	}
	want := strings.Replace(dependencyRevisionGoldenPreimage, "%s", configDigest, 1)

	got, err := dependencyRevisionPreimage(inputs)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("canonical encoding drifted\n got: %q\nwant: %q", got, want)
	}

	sum := sha256.Sum256([]byte(want))
	wantRevision := "cohort-v1:" + hex.EncodeToString(sum[:])
	if revision := mustComputeDependencyRevision(t, inputs); revision != wantRevision {
		t.Fatalf("revision = %q, want %q", revision, wantRevision)
	}
}

// TestDependencyRevisionEncodingIsLengthDelimited is the delimiter check the
// plan names: two cohorts that differ only in where one field ends must not
// collide, and a value that carries the encoding's own separators must be
// written with its length so it cannot be read as more than one field.
//
// The length prefix is checked directly as well as through its consequence.
// Labels and list-count headers already keep most shapes apart, so dropping the
// prefix does not immediately produce a colliding pair to point at — but it
// does silently re-key every stored generation, which is what the assertion on
// the rendered field catches.
func TestDependencyRevisionEncodingIsLengthDelimited(t *testing.T) {
	left := dependencyRevisionCohort()
	left.Target.WorkspaceID = "a\x00b"
	left.Target.ProjectID = "c"

	right := dependencyRevisionCohort()
	right.Target.WorkspaceID = "a"
	right.Target.ProjectID = "b\x00c"

	if mustComputeDependencyRevision(t, left) == mustComputeDependencyRevision(t, right) {
		t.Fatal("two distinct cohorts collide: the encoding is not length-delimited")
	}

	// The same argument inside a list: a root identity that carries the
	// following field's separator must not be able to spell that field.
	shifted := dependencyRevisionCohort()
	shifted.Repositories[1].RootIdentity = "root-b\x00repo.source:5:src-b"
	shifted.Repositories[1].SourceIdentity = "src-b"
	if mustComputeDependencyRevision(t, shifted) == mustComputeDependencyRevision(t, dependencyRevisionCohort()) {
		t.Fatal("a label-shaped value impersonated the next field")
	}

	// And the prefix itself: a three-byte value carrying a NUL is written as
	// three bytes, not as two fields.
	preimage, err := dependencyRevisionPreimage(left)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preimage, "target.workspace:3:a\x00b\x00") {
		t.Fatalf("field is not length-delimited: %q", preimage)
	}
	if !strings.Contains(preimage, "roster.count:1:2\x00") {
		t.Fatalf("list length is not part of the encoding: %q", preimage)
	}
}

// TestComputeDependencyRevisionFailsClosedOnIncompleteCohort covers hazard H2:
// an incomplete cohort must produce no revision at all rather than a digest
// that reads as a freshness certificate. The empty revision it leaves behind is
// exactly the legacy identity generationIdentityKey already renders.
func TestComputeDependencyRevisionFailsClosedOnIncompleteCohort(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*DependencyRevisionInputs)
	}{
		{"roster not asserted complete", func(in *DependencyRevisionInputs) {
			in.RosterComplete = false
		}},
		{"empty roster", func(in *DependencyRevisionInputs) {
			in.Repositories = nil
		}},
		{"no target repository", func(in *DependencyRevisionInputs) {
			in.Target.RepoPrefix = ""
		}},
		{"target outside the roster", func(in *DependencyRevisionInputs) {
			in.Target.RepoPrefix = "gamma"
		}},
		{"repository without a source identity", func(in *DependencyRevisionInputs) {
			in.Repositories[1].SourceIdentity = ""
		}},
		{"repository without an incarnation", func(in *DependencyRevisionInputs) {
			in.Repositories[1].Incarnation = ""
		}},
		{"repository without a prefix", func(in *DependencyRevisionInputs) {
			in.Repositories[1].RepoPrefix = ""
		}},
		{"repository of unknown kind", func(in *DependencyRevisionInputs) {
			in.Repositories[1].Kind = "borrowed"
		}},
		{"dedicated repository without a graph", func(in *DependencyRevisionInputs) {
			in.Repositories[0].GraphID = ""
		}},
		{"raw repository without a root", func(in *DependencyRevisionInputs) {
			in.Repositories[1].RootIdentity = ""
		}},
		{"repository listed twice", func(in *DependencyRevisionInputs) {
			in.Repositories = append(in.Repositories, in.Repositories[1])
		}},
		{"ownership outside the roster", func(in *DependencyRevisionInputs) {
			in.Ownership[0].RepoPrefix = "gamma"
		}},
		{"ownership without an owner", func(in *DependencyRevisionInputs) {
			in.Ownership[0].Owner = ""
		}},
		{"contradicting ownership", func(in *DependencyRevisionInputs) {
			contradiction := in.Ownership[0]
			contradiction.Owner = "example.com/other"
			in.Ownership = append(in.Ownership, contradiction)
		}},
		{"ownership not asserted complete", func(in *DependencyRevisionInputs) {
			in.OwnershipComplete = false
		}},
		{"configuration section without a digest", func(in *DependencyRevisionInputs) {
			in.ConfigSections[0].Digest = ""
		}},
		{"configuration section listed twice", func(in *DependencyRevisionInputs) {
			in.ConfigSections = append(in.ConfigSections,
				DependencyRevisionConfigSection{Name: DependencyRevisionConfigLSP, Digest: "other"})
		}},
		{"no configuration sections at all", func(in *DependencyRevisionInputs) {
			in.ConfigSections = nil
		}},
		{"no producer policy", func(in *DependencyRevisionInputs) {
			in.Producers = nil
		}},
		{"producer without a state", func(in *DependencyRevisionInputs) {
			in.Producers[0].State = ""
		}},
		{"producer declared twice", func(in *DependencyRevisionInputs) {
			in.Producers = append(in.Producers, DependencyRevisionProducer{
				Producer: string(graphview.CapSearchText), State: "complete"})
		}},
		{"no capability set", func(in *DependencyRevisionInputs) {
			in.Capabilities = nil
		}},
		{"undefined capability", func(in *DependencyRevisionInputs) {
			in.Capabilities = append(in.Capabilities, "search.telepathy")
		}},
		{"no extractor versions", func(in *DependencyRevisionInputs) {
			in.ExtractorVersions = ""
		}},
	}
	for _, tc := range cases {
		inputs := dependencyRevisionCohort()
		tc.apply(&inputs)
		revision, err := ComputeDependencyRevision(inputs)
		if !errors.Is(err, ErrDependencyRevisionIncomplete) {
			t.Errorf("%s: err = %v, want ErrDependencyRevisionIncomplete", tc.name, err)
		}
		if revision != "" {
			t.Errorf("%s: refusal produced revision %q", tc.name, revision)
		}
	}

	// The refusal's empty revision is the legacy identity, byte for byte.
	identity := GenerationIdentity{OwnerKind: "owner", GraphID: "graph", LayerID: "layer",
		CheckoutID: "checkout", GenerationKind: "kind", BaseGenerationID: 7,
		LowerViewFingerprint: "lower", TreeOID: "tree", ProvenanceCommitOID: "commit",
		ConfigHash: "config", ExtractorVersions: "extractors", ResolverVersion: "resolver"}
	legacy := generationIdentityKey(identity)
	refused := dependencyRevisionCohort()
	refused.RosterComplete = false
	revision, _ := ComputeDependencyRevision(refused)
	identity.DependencyRevision = revision
	if got := generationIdentityKey(identity); got != legacy {
		t.Fatalf("a refused cohort left a non-legacy identity: %q != %q", got, legacy)
	}
}

// TestDependencyRevisionRosterReadsTheCompleteRegisteredRoster proves the
// producer's roster half against the real lease manager rather than a stub: the
// enumerator is graphview.AcquireRepositoryRoster, dedicated and raw owners
// both land in the cohort, and a roster that moved under the lease is refused.
func TestDependencyRevisionRosterReadsTheCompleteRegisteredRoster(t *testing.T) {
	manager := graphview.NewLeaseManager()
	dedicated := graphview.RepositoryOwner{
		GraphID: "g-a", CheckoutID: "co-a", Incarnation: "inc-a", RepoPrefix: "alpha"}
	if err := manager.RegisterRepositoryOwner(dedicated); err != nil {
		t.Fatal(err)
	}
	raw := graphview.RawRepositoryOwner{
		RepoPrefix: "beta", RootIdentity: "root-b", Incarnation: "inc-b"}
	if _, err := manager.RegisterRawRepositoryOwnerPrepared(raw, nil); err != nil {
		t.Fatal(err)
	}

	lease, err := manager.AcquireRepositoryRoster()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	sources := map[string]string{"alpha": "src-a", "beta": "src-b"}
	identity := func(prefix string) string { return sources[prefix] }

	repositories, err := DependencyRevisionRoster(lease, identity)
	if err != nil {
		t.Fatal(err)
	}
	want := []DependencyRevisionRepository{
		{RepoPrefix: "alpha", Kind: DependencyRevisionRepositoryDedicated,
			GraphID: "g-a", CheckoutID: "co-a", Incarnation: "inc-a", SourceIdentity: "src-a"},
		{RepoPrefix: "beta", Kind: DependencyRevisionRepositoryRaw,
			Incarnation: "inc-b", RootIdentity: "root-b", SourceIdentity: "src-b"},
	}
	if !slices.Equal(repositories, want) {
		t.Fatalf("roster = %+v, want %+v", repositories, want)
	}

	// The roster feeds a complete cohort, and the cross-repository member is
	// carried with its own source identity.
	inputs := dependencyRevisionCohort()
	inputs.Repositories = repositories
	if got := mustComputeDependencyRevision(t, inputs); got != mustComputeDependencyRevision(t, dependencyRevisionCohort()) {
		t.Fatal("the lease-derived roster is not the fixture cohort")
	}

	// A repository the resolver can see but the cohort cannot describe refuses
	// the whole roster: a partial cohort is a false certificate.
	if _, err := DependencyRevisionRoster(lease, func(string) string { return "" }); err == nil {
		t.Fatal("a roster with no source identity was accepted")
	}
	if _, err := DependencyRevisionRoster(lease, nil); !errors.Is(err, ErrDependencyRevisionIncomplete) {
		t.Fatalf("nil source identity err = %v", err)
	}
	if _, err := DependencyRevisionRoster(nil, identity); !errors.Is(err, ErrDependencyRevisionIncomplete) {
		t.Fatalf("nil lease err = %v", err)
	}

	// A roster that gained a repository after the lease was taken is stale, and
	// the digest must not be computed over the moment that has passed.
	late := graphview.RawRepositoryOwner{
		RepoPrefix: "gamma", RootIdentity: "root-c", Incarnation: "inc-c"}
	if _, err := manager.RegisterRawRepositoryOwnerPrepared(late, nil); err != nil {
		t.Fatal(err)
	}
	repositories, err = DependencyRevisionRoster(lease, identity)
	if !errors.Is(err, ErrDependencyRevisionIncomplete) {
		t.Fatalf("a moved roster was accepted: err = %v", err)
	}
	if repositories != nil {
		t.Fatalf("a refused roster returned %d entries", len(repositories))
	}
}

// TestDependencyRevisionSourceIdentityMissingForOneRepositoryRefuses keeps the
// per-repository half of the completeness rule honest: it is not enough for the
// target's bytes to be named — every repository the resolver can see must be.
func TestDependencyRevisionSourceIdentityMissingForOneRepositoryRefuses(t *testing.T) {
	manager := graphview.NewLeaseManager()
	for _, owner := range []graphview.RawRepositoryOwner{
		{RepoPrefix: "alpha", RootIdentity: "root-a", Incarnation: "inc-a"},
		{RepoPrefix: "beta", RootIdentity: "root-b", Incarnation: "inc-b"},
	} {
		if _, err := manager.RegisterRawRepositoryOwnerPrepared(owner, nil); err != nil {
			t.Fatal(err)
		}
	}
	lease, err := manager.AcquireRepositoryRoster()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	partial := map[string]string{"alpha": "src-a"}
	repositories, err := DependencyRevisionRoster(lease, func(prefix string) string { return partial[prefix] })
	if !errors.Is(err, ErrDependencyRevisionIncomplete) {
		t.Fatalf("a roster with one unnamed repository was accepted: err = %v", err)
	}
	if !strings.Contains(err.Error(), `"beta"`) {
		t.Fatalf("refusal does not name the repository it could not describe: %v", err)
	}
	if repositories != nil {
		t.Fatalf("a refused roster returned %d entries", len(repositories))
	}

	// The same cohort with beta's bytes named is a revision, so the refusal
	// above is the missing identity and not the roster itself.
	named := map[string]string{"alpha": "src-a", "beta": "src-b"}
	repositories, err = DependencyRevisionRoster(lease, func(prefix string) string { return named[prefix] })
	if err != nil {
		t.Fatal(err)
	}
	inputs := dependencyRevisionCohort()
	inputs.Repositories = repositories
	inputs.Ownership = nil
	if revision := mustComputeDependencyRevision(t, inputs); revision == "" {
		t.Fatal("a complete cohort produced no revision")
	}
}

// TestComputeDependencyRevisionRequiresEveryConfigurationDomain pins the
// required non-index configuration domains one at a time, so removing a domain
// from dependencyRevisionRequiredConfigDomains fails here rather than silently
// letting a cohort that never enumerated it still certify frozen inputs.
func TestComputeDependencyRevisionRequiresEveryConfigurationDomain(t *testing.T) {
	required := []string{
		DependencyRevisionConfigArtifacts,
		DependencyRevisionConfigLSP,
		DependencyRevisionConfigProject,
		DependencyRevisionConfigSemantic,
		DependencyRevisionConfigWorkspace,
	}
	for _, domain := range required {
		inputs := dependencyRevisionCohort()
		inputs.ConfigSections = slices.DeleteFunc(
			slices.Clone(inputs.ConfigSections),
			func(section DependencyRevisionConfigSection) bool { return section.Name == domain },
		)
		if len(inputs.ConfigSections) != len(dependencyRevisionCohort().ConfigSections)-1 {
			t.Fatalf("%s: fixture does not describe the domain, so its absence proves nothing", domain)
		}
		revision, err := ComputeDependencyRevision(inputs)
		if !errors.Is(err, ErrDependencyRevisionIncomplete) {
			t.Errorf("%s: omitting the domain produced err = %v, want ErrDependencyRevisionIncomplete",
				domain, err)
		}
		if revision != "" {
			t.Errorf("%s: a cohort missing the domain produced revision %q", domain, revision)
		}
		if err != nil && !strings.Contains(err.Error(), domain) {
			t.Errorf("%s: refusal does not name the missing domain: %v", domain, err)
		}
	}

	// A domain outside the required set is still digested, so the requirement
	// is a floor and not a closed vocabulary.
	extended := dependencyRevisionCohort()
	extended.ConfigSections = append(extended.ConfigSections,
		DependencyRevisionConfigSection{Name: "vector", Digest: "vec-digest"})
	if mustComputeDependencyRevision(t, extended) ==
		mustComputeDependencyRevision(t, dependencyRevisionCohort()) {
		t.Fatal("an additional configuration domain did not change the revision")
	}
}

// TestDependencyRevisionRosterRefusesAnEmptyRegisteredRoster pins the refusal
// at the enumeration boundary: a lease over a lease manager with no registered
// repository validates fine (nothing moved under it), so without the explicit
// check DependencyRevisionRoster would hand back an empty, perfectly valid
// cohort for the digest to certify.
func TestDependencyRevisionRosterRefusesAnEmptyRegisteredRoster(t *testing.T) {
	manager := graphview.NewLeaseManager()
	lease, err := manager.AcquireRepositoryRoster()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	// The lease itself is current: the refusal below is the empty roster and
	// not a stale-lease rejection.
	if err := lease.ValidateCurrent(); err != nil {
		t.Fatalf("an empty roster lease is not current: %v", err)
	}

	repositories, err := DependencyRevisionRoster(lease, func(string) string { return "src" })
	if !errors.Is(err, ErrDependencyRevisionIncomplete) {
		t.Fatalf("an empty registered roster was accepted: err = %v", err)
	}
	if !strings.Contains(err.Error(), "no repository") {
		t.Fatalf("refusal does not say the roster is empty: %v", err)
	}
	if repositories != nil {
		t.Fatalf("a refused roster returned %d entries", len(repositories))
	}
}

// TestDependencyRevisionRosterScopedNarrowsToTheAdmittedRepositories pins the
// scoped enumeration.
//
// The narrowing is what stops a commit in an unrelated repository from
// re-keying — and therefore rebuilding — every checkout layer in the daemon.
// What it must NOT relax is the completeness rule for the repositories it does
// admit, or the staleness check over the whole live registration set.
func TestDependencyRevisionRosterScopedNarrowsToTheAdmittedRepositories(t *testing.T) {
	manager := graphview.NewLeaseManager()
	dedicated := graphview.RepositoryOwner{
		GraphID: "g-a", CheckoutID: "co-a", Incarnation: "inc-a", RepoPrefix: "alpha"}
	if err := manager.RegisterRepositoryOwner(dedicated); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []graphview.RawRepositoryOwner{
		{RepoPrefix: "beta", RootIdentity: "root-b", Incarnation: "inc-b"},
		{RepoPrefix: "gamma", RootIdentity: "root-c", Incarnation: "inc-c"},
	} {
		if _, err := manager.RegisterRawRepositoryOwnerPrepared(raw, nil); err != nil {
			t.Fatal(err)
		}
	}
	lease, err := manager.AcquireRepositoryRoster()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	// Only the admitted repositories' bytes are named; the others are not even
	// asked for, which is both the correctness and the cost half.
	asked := map[string]int{}
	identity := func(prefix string) string {
		asked[prefix]++
		return "src-" + prefix
	}
	inScope := func(prefix string) bool { return prefix == "alpha" || prefix == "beta" }

	repositories, err := DependencyRevisionRosterScoped(lease, inScope, identity)
	if err != nil {
		t.Fatal(err)
	}
	want := []DependencyRevisionRepository{
		{RepoPrefix: "alpha", Kind: DependencyRevisionRepositoryDedicated,
			GraphID: "g-a", CheckoutID: "co-a", Incarnation: "inc-a", SourceIdentity: "src-alpha"},
		{RepoPrefix: "beta", Kind: DependencyRevisionRepositoryRaw,
			Incarnation: "inc-b", RootIdentity: "root-b", SourceIdentity: "src-beta"},
	}
	if !slices.Equal(repositories, want) {
		t.Fatalf("scoped roster = %+v, want %+v", repositories, want)
	}
	if asked["gamma"] != 0 {
		t.Fatalf("an out-of-scope repository's source was read %d times", asked["gamma"])
	}

	// The completeness rule still holds inside the scope.
	if _, err := DependencyRevisionRosterScoped(lease, inScope, func(prefix string) string {
		if prefix == "beta" {
			return ""
		}
		return "src-" + prefix
	}); !errors.Is(err, ErrDependencyRevisionIncomplete) {
		t.Fatalf("an in-scope repository with no source identity was accepted: err = %v", err)
	}

	// A scope that admits nothing is a refusal, not an empty certificate.
	if _, err := DependencyRevisionRosterScoped(lease, func(string) bool { return false },
		identity); !errors.Is(err, ErrDependencyRevisionIncomplete) {
		t.Fatalf("a scope that admits no repository was accepted: err = %v", err)
	}

	// And the staleness check is still over the WHOLE live registration set: a
	// repository that joined after the lease was taken refuses the digest even
	// when the scope would not have admitted it, because the lease no longer
	// describes a moment that happened.
	late := graphview.RawRepositoryOwner{
		RepoPrefix: "delta", RootIdentity: "root-d", Incarnation: "inc-d"}
	if _, err := manager.RegisterRawRepositoryOwnerPrepared(late, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := DependencyRevisionRosterScoped(lease, inScope, identity); !errors.Is(
		err, ErrDependencyRevisionIncomplete) {
		t.Fatalf("a moved roster was accepted under a scope: err = %v", err)
	}
}

// TestDependencyRevisionScopeIsPartOfTheCertificate pins the declared scope as
// a digested input. Without it two cohorts that enumerated different worlds —
// a whole daemon and one workspace — could produce one revision whenever their
// member lists happened to coincide, and a reader comparing revisions could not
// tell which claim it was holding.
func TestDependencyRevisionScopeIsPartOfTheCertificate(t *testing.T) {
	unscoped := dependencyRevisionCohort()
	baseline := mustComputeDependencyRevision(t, unscoped)

	workspace := dependencyRevisionCohort()
	workspace.RosterScope = DependencyRevisionScopeWorkspace + "ws"
	scoped := mustComputeDependencyRevision(t, workspace)
	if scoped == baseline {
		t.Fatal("declaring a roster scope left the revision where it was")
	}

	repository := dependencyRevisionCohort()
	repository.RosterScope = DependencyRevisionScopeRepository + "alpha"
	if narrow := mustComputeDependencyRevision(t, repository); narrow == scoped {
		t.Fatal("two different roster scopes produced one revision")
	}
}

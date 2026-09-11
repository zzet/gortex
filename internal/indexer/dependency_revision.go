package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graphview"
)

// DependencyRevisionEncodingVersion prefixes every revision this package
// produces. It is part of the produced string, not only of the preimage, so a
// stored revision names the encoding that made it and a future encoding cannot
// be mistaken for this one by a reader comparing two revisions for equality.
//
// Bump it in the same change that alters the canonical encoding below. The
// existing catalog and identity fixtures already speak this vocabulary
// (`store_sqlite/catalog_dependency_revision_test.go`), so the prefix is the
// established one rather than a new dialect.
const DependencyRevisionEncodingVersion = "cohort-v1"

// Repository kinds a cohort member can have. A dedicated repository is one the
// lease manager admits with a graph of its own; a raw repository is an external
// checkout admitted by root identity. The kind is digested, so re-admitting the
// same prefix under a different kind is a different cohort.
const (
	DependencyRevisionRepositoryDedicated = "dedicated"
	DependencyRevisionRepositoryRaw       = "raw"
)

// ErrDependencyRevisionIncomplete means the cohort handed to
// ComputeDependencyRevision does not describe the complete resolver-visible
// input set, so no revision was produced.
//
// This is the fail-closed half of the freshness contract: a digest over a
// partial roster is a false certificate — it reads as "these inputs are frozen"
// while a repository the resolver can actually see is missing from it. Callers
// must treat the error as "no revision", leave `GenerationIdentity.Dependency-
// Revision` empty (which `generationIdentityKey` keeps legacy-compatible) and
// forgo the reuse the revision would have certified.
var ErrDependencyRevisionIncomplete = errors.New("indexer: incomplete dependency-revision cohort")

// DependencyRevisionTarget names the repository a build is for. It is the
// cohort's anchor: the target must itself be a roster member, which is the one
// completeness claim this package can check rather than take on trust.
type DependencyRevisionTarget struct {
	RepoPrefix  string
	WorkspaceID string
	ProjectID   string
}

// DependencyRevisionRepository is one member of the resolver-visible roster,
// including a cross-repository input the target resolves against.
//
// SourceIdentity is what the build can actually see of that repository's bytes
// — a tree OID for a committed source, a content fingerprint for a working
// copy. It is required for every member: a roster entry with no source identity
// says "this repository is in scope but I cannot name what it contained", which
// is exactly the certificate this digest must not issue.
type DependencyRevisionRepository struct {
	RepoPrefix     string
	Kind           string
	GraphID        string
	CheckoutID     string
	Incarnation    string
	RootIdentity   string
	SourceIdentity string
}

// DependencyRevisionOwnership is one ownership fact the resolver consults —
// the Go module/package ownership evidence a candidate gate reads
// (`resolver.GoPackageOwnershipLookup`) and its equivalents.
//
// Scope is the repo-relative directory the fact covers (empty means the whole
// repository); Owner is the module/package identity that owns it. A fact whose
// repository is not a roster member is refused: naming ownership in a
// repository the cohort does not carry is the same false certificate as an
// incomplete roster.
type DependencyRevisionOwnership struct {
	RepoPrefix string
	Language   string
	Scope      string
	Owner      string
}

// DependencyRevisionProducer is one row of the producer policy the build will
// declare (`SparseGenerationBuilder.declareProducers`). The reason is digested
// with the state: a producer that is complete for one reason and complete for
// another describes the same payload, but a policy whose *wording* changed is
// still a policy change a reader may act on, and the cheap conservative choice
// is to invalidate.
type DependencyRevisionProducer struct {
	Producer string
	State    string
	Reason   string
}

// DependencyRevisionConfigSection carries a configuration domain that is not
// part of `config.IndexConfig` — artifacts, semantic, LSP, workspace overlays —
// as a name and a digest the caller already computed.
//
// The index configuration is digested here in full (see the Config field);
// everything else is named rather than embedded so this package does not have
// to grow a dependency on every configuration struct in the tree. The list is
// digested by name, so a section that appears later changes the revision.
//
// The domains in dependencyRevisionRequiredConfigDomains must all be present:
// see that variable for why silence about a domain is refused rather than
// digested as an absence.
type DependencyRevisionConfigSection struct {
	Name   string
	Digest string
}

// The configuration domains outside config.IndexConfig that the resolver can
// see. Every one of them must appear in DependencyRevisionInputs.ConfigSections.
const (
	DependencyRevisionConfigArtifacts = "artifacts"
	DependencyRevisionConfigLSP       = "lsp"
	DependencyRevisionConfigProject   = "project"
	DependencyRevisionConfigSemantic  = "semantic"
	DependencyRevisionConfigWorkspace = "workspace"
)

// dependencyRevisionRequiredConfigDomains is the closed set of non-index
// configuration domains a cohort must describe.
//
// Without it the fail-closed posture is asymmetric: an incomplete producer
// policy or capability set is refused, but a caller that simply omitted every
// non-index configuration domain still receives a revision — a certificate
// saying "these inputs are frozen" over inputs nobody enumerated. Naming the
// domains turns that from an undetectable caller bug into a refusal here, so
// the wiring item cannot forget a domain silently. A caller that genuinely has
// no configuration for a domain still declares it, with the digest of its empty
// value; a domain that is not in this list may be declared as well, and is
// digested like any other.
var dependencyRevisionRequiredConfigDomains = []string{
	DependencyRevisionConfigArtifacts,
	DependencyRevisionConfigLSP,
	DependencyRevisionConfigProject,
	DependencyRevisionConfigSemantic,
	DependencyRevisionConfigWorkspace,
}

// DependencyRevisionInputs is the complete resolver-visible input cohort.
//
// Ordering does not matter: every list is sorted into a canonical order before
// encoding, so two callers that enumerate the same roster in different orders
// produce the same revision. Duplicates that disagree are refused rather than
// silently collapsed.
type DependencyRevisionInputs struct {
	// Target is the repository the generation is being built for.
	Target DependencyRevisionTarget

	// RosterComplete is the caller's assertion that Repositories is the
	// complete registered roster — the one it just validated under the lease
	// that produced it, not a filtered or cached subset. False refuses.
	RosterComplete bool

	// Repositories is the roster, in any order.
	Repositories []DependencyRevisionRepository

	// OwnershipComplete is the caller's assertion that Ownership is the
	// complete ownership evidence the resolver will consult. False refuses.
	//
	// It exists because an empty Ownership list is ambiguous on its own: a
	// cohort with no module manifests genuinely has no ownership facts, and a
	// caller that forgot to enumerate them also has none. Only the caller can
	// tell the two apart, so it says which — an absence that was measured is a
	// fact about the cohort, an absence nobody looked for is a gap, and the
	// digest must not read the second as the first.
	OwnershipComplete bool

	// Ownership is the ownership evidence the resolver will consult, in any
	// order. It may be empty when OwnershipComplete asserts the emptiness was
	// measured.
	Ownership []DependencyRevisionOwnership

	// Config is the frozen effective index configuration — the value
	// snapshotDedicatedBaseConfig returned, not the live ConfigManager's
	// shallow result. It is fingerprinted here by that same function, so the
	// revision and the dedicated-base config fingerprint can never drift.
	Config config.IndexConfig

	// ConfigSections carries the remaining configuration domains by name.
	ConfigSections []DependencyRevisionConfigSection

	// Producers is the producer policy the build will declare, in any order.
	Producers []DependencyRevisionProducer

	// Capabilities is the capability vocabulary the policy is stated over.
	// Every entry must be a defined capability id.
	Capabilities []string

	// ExtractorVersions is the extractor policy fingerprint, rendered the way
	// the generation identity's own column renders it
	// (`extractorVersionsFingerprint`).
	ExtractorVersions string
}

// ComputeDependencyRevision digests the complete resolver-visible input cohort
// into one comparable revision string.
//
// The result is `cohort-v1:<sha256 hex>` over a canonical, length-delimited
// encoding of every cohort member. Determinism is by construction: every list
// is sorted, no map is ranged over, and every value is length-prefixed so no
// value can imitate a delimiter and make two distinct cohorts collide.
//
// It fails closed. Every refusal wraps ErrDependencyRevisionIncomplete and
// returns an empty revision, which the identity key treats as the legacy
// identity — a build that cannot describe its inputs completely does not get
// to claim they are frozen.
//
// The resolver version is deliberately NOT a member: it is its own generation-
// identity column, and digesting it here would make one input change two
// identity fields.
func ComputeDependencyRevision(inputs DependencyRevisionInputs) (string, error) {
	preimage, err := dependencyRevisionPreimage(inputs)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(preimage))
	return DependencyRevisionEncodingVersion + ":" + hex.EncodeToString(sum[:]), nil
}

// DependencyRevisionRoster reads the complete registered roster out of a held
// roster lease.
//
// The lease is re-validated first: `RepositoryRosterLease.ValidateCurrent` is a
// comparison against the live registration set, so a roster that gained or lost
// a repository since the lease was taken is refused here rather than digested.
// The caller must hold the lease across the validation, the digest and the
// catalog transaction that stores the revision — this function checks a moment,
// it does not hold one.
//
// sourceIdentity names what the build can see of each repository. It must
// return a non-empty identity for every roster member; returning empty for one
// refuses the whole roster, because a cohort that carries a repository it
// cannot describe is not a certificate.
func DependencyRevisionRoster(
	lease *graphview.RepositoryRosterLease,
	sourceIdentity func(repoPrefix string) string,
) ([]DependencyRevisionRepository, error) {
	if lease == nil {
		return nil, fmt.Errorf("%w: no roster lease", ErrDependencyRevisionIncomplete)
	}
	if sourceIdentity == nil {
		return nil, fmt.Errorf("%w: no source identity for the roster", ErrDependencyRevisionIncomplete)
	}
	if err := lease.ValidateCurrent(); err != nil {
		return nil, fmt.Errorf("%w: roster is no longer current: %w", ErrDependencyRevisionIncomplete, err)
	}
	dedicated := lease.DedicatedOwners()
	raw := lease.RawRegistrations()
	out := make([]DependencyRevisionRepository, 0, len(dedicated)+len(raw))
	for _, owner := range dedicated {
		out = append(out, DependencyRevisionRepository{
			RepoPrefix:     owner.RepoPrefix,
			Kind:           DependencyRevisionRepositoryDedicated,
			GraphID:        owner.GraphID,
			CheckoutID:     owner.CheckoutID,
			Incarnation:    owner.Incarnation,
			SourceIdentity: sourceIdentity(owner.RepoPrefix),
		})
	}
	for _, registration := range raw {
		owner := registration.Owner()
		out = append(out, DependencyRevisionRepository{
			RepoPrefix:     owner.RepoPrefix,
			Kind:           DependencyRevisionRepositoryRaw,
			Incarnation:    owner.Incarnation,
			RootIdentity:   owner.RootIdentity,
			SourceIdentity: sourceIdentity(owner.RepoPrefix),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: roster lease carries no repository", ErrDependencyRevisionIncomplete)
	}
	// Refuse at the boundary that read the roster, not only in the digest: the
	// caller that cannot name one repository's bytes has an incomplete cohort
	// here, and the closer the refusal is to the enumeration the harder it is
	// to paper over with a filtered list.
	for _, repository := range out {
		if repository.SourceIdentity == "" {
			return nil, fmt.Errorf("%w: repository %q has no source identity",
				ErrDependencyRevisionIncomplete, repository.RepoPrefix)
		}
	}
	return out, nil
}

// dependencyRevisionPreimage renders the cohort as the canonical byte string
// the revision digests. It is separate from the hashing so a test can pin the
// encoding itself: a delimiter or ordering change is visible here as a diff
// rather than only as a changed hex string.
func dependencyRevisionPreimage(inputs DependencyRevisionInputs) (string, error) {
	repositories, err := canonicalDependencyRevisionRoster(inputs)
	if err != nil {
		return "", err
	}
	members := make(map[string]struct{}, len(repositories))
	for _, repository := range repositories {
		members[repository.RepoPrefix] = struct{}{}
	}
	if _, ok := members[inputs.Target.RepoPrefix]; !ok {
		return "", fmt.Errorf("%w: target repository %q is not a roster member",
			ErrDependencyRevisionIncomplete, inputs.Target.RepoPrefix)
	}
	ownership, err := canonicalDependencyRevisionOwnership(
		inputs.Ownership, members, inputs.OwnershipComplete)
	if err != nil {
		return "", err
	}
	sections, err := canonicalDependencyRevisionConfigSections(inputs.ConfigSections)
	if err != nil {
		return "", err
	}
	producers, err := canonicalDependencyRevisionProducers(inputs.Producers)
	if err != nil {
		return "", err
	}
	capabilities, err := canonicalDependencyRevisionCapabilities(inputs.Capabilities)
	if err != nil {
		return "", err
	}
	if inputs.ExtractorVersions == "" {
		return "", fmt.Errorf("%w: no extractor versions", ErrDependencyRevisionIncomplete)
	}
	// The config is fingerprinted by the same function that freezes it for the
	// dedicated base, so the revision cannot describe a configuration the
	// snapshot does not. An unencodable configuration is a refusal here rather
	// than the unique-digest fallback indexConfigHash uses: that fallback makes
	// every build its own identity, which is the right answer for a cache key
	// and the wrong one for a frozen-input certificate.
	_, configDigest, err := snapshotDedicatedBaseConfig(
		inputs.Config, inputs.Target.RepoPrefix, inputs.Target.WorkspaceID, inputs.Target.ProjectID)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrDependencyRevisionIncomplete, err)
	}

	var e dependencyRevisionEncoder
	e.field("encoding", DependencyRevisionEncodingVersion)
	e.field("target.repo", inputs.Target.RepoPrefix)
	e.field("target.workspace", inputs.Target.WorkspaceID)
	e.field("target.project", inputs.Target.ProjectID)

	e.count("roster", len(repositories))
	for _, repository := range repositories {
		e.field("repo.prefix", repository.RepoPrefix)
		e.field("repo.kind", repository.Kind)
		e.field("repo.graph", repository.GraphID)
		e.field("repo.checkout", repository.CheckoutID)
		e.field("repo.incarnation", repository.Incarnation)
		e.field("repo.root", repository.RootIdentity)
		e.field("repo.source", repository.SourceIdentity)
	}

	e.count("ownership", len(ownership))
	for _, fact := range ownership {
		e.field("own.repo", fact.RepoPrefix)
		e.field("own.language", fact.Language)
		e.field("own.scope", fact.Scope)
		e.field("own.owner", fact.Owner)
	}

	e.field("config.index", configDigest)
	e.count("config.sections", len(sections))
	for _, section := range sections {
		e.field("config.section.name", section.Name)
		e.field("config.section.digest", section.Digest)
	}

	e.count("producers", len(producers))
	for _, producer := range producers {
		e.field("producer.id", producer.Producer)
		e.field("producer.state", producer.State)
		e.field("producer.reason", producer.Reason)
	}

	e.count("capabilities", len(capabilities))
	for _, capability := range capabilities {
		e.field("capability", capability)
	}

	e.field("extractors", inputs.ExtractorVersions)
	return e.b.String(), nil
}

// canonicalDependencyRevisionRoster validates and sorts the roster.
func canonicalDependencyRevisionRoster(
	inputs DependencyRevisionInputs,
) ([]DependencyRevisionRepository, error) {
	if inputs.Target.RepoPrefix == "" {
		return nil, fmt.Errorf("%w: no target repository", ErrDependencyRevisionIncomplete)
	}
	if !inputs.RosterComplete {
		return nil, fmt.Errorf("%w: roster is not asserted complete", ErrDependencyRevisionIncomplete)
	}
	if len(inputs.Repositories) == 0 {
		return nil, fmt.Errorf("%w: empty roster", ErrDependencyRevisionIncomplete)
	}
	out := slices.Clone(inputs.Repositories)
	sort.SliceStable(out, func(i, j int) bool {
		return dependencyRevisionRepositoryLess(out[i], out[j])
	})
	seen := make(map[string]struct{}, len(out))
	for _, repository := range out {
		switch {
		case repository.RepoPrefix == "":
			return nil, fmt.Errorf("%w: roster entry with no repository prefix", ErrDependencyRevisionIncomplete)
		case repository.SourceIdentity == "":
			return nil, fmt.Errorf("%w: repository %q has no source identity",
				ErrDependencyRevisionIncomplete, repository.RepoPrefix)
		case repository.Incarnation == "":
			return nil, fmt.Errorf("%w: repository %q has no incarnation",
				ErrDependencyRevisionIncomplete, repository.RepoPrefix)
		}
		switch repository.Kind {
		case DependencyRevisionRepositoryDedicated:
			if repository.GraphID == "" || repository.CheckoutID == "" {
				return nil, fmt.Errorf("%w: dedicated repository %q has no graph/checkout identity",
					ErrDependencyRevisionIncomplete, repository.RepoPrefix)
			}
		case DependencyRevisionRepositoryRaw:
			if repository.RootIdentity == "" {
				return nil, fmt.Errorf("%w: raw repository %q has no root identity",
					ErrDependencyRevisionIncomplete, repository.RepoPrefix)
			}
		default:
			return nil, fmt.Errorf("%w: repository %q has unknown kind %q",
				ErrDependencyRevisionIncomplete, repository.RepoPrefix, repository.Kind)
		}
		if _, duplicate := seen[repository.RepoPrefix]; duplicate {
			return nil, fmt.Errorf("%w: repository %q listed twice",
				ErrDependencyRevisionIncomplete, repository.RepoPrefix)
		}
		seen[repository.RepoPrefix] = struct{}{}
	}
	return out, nil
}

func dependencyRevisionRepositoryLess(a, b DependencyRevisionRepository) bool {
	if a.RepoPrefix != b.RepoPrefix {
		return a.RepoPrefix < b.RepoPrefix
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.GraphID != b.GraphID {
		return a.GraphID < b.GraphID
	}
	if a.CheckoutID != b.CheckoutID {
		return a.CheckoutID < b.CheckoutID
	}
	if a.Incarnation != b.Incarnation {
		return a.Incarnation < b.Incarnation
	}
	if a.RootIdentity != b.RootIdentity {
		return a.RootIdentity < b.RootIdentity
	}
	return a.SourceIdentity < b.SourceIdentity
}

// canonicalDependencyRevisionOwnership validates and sorts the ownership facts.
// Two facts that cover the same repository, language and scope but name
// different owners are a contradiction the digest must not average away.
func canonicalDependencyRevisionOwnership(
	facts []DependencyRevisionOwnership, members map[string]struct{}, complete bool,
) ([]DependencyRevisionOwnership, error) {
	if !complete {
		return nil, fmt.Errorf("%w: ownership is not asserted complete",
			ErrDependencyRevisionIncomplete)
	}
	out := slices.Clone(facts)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].RepoPrefix != out[j].RepoPrefix {
			return out[i].RepoPrefix < out[j].RepoPrefix
		}
		if out[i].Language != out[j].Language {
			return out[i].Language < out[j].Language
		}
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Owner < out[j].Owner
	})
	type ownershipKey struct{ repo, language, scope string }
	owners := make(map[ownershipKey]string, len(out))
	deduped := out[:0]
	for _, fact := range out {
		if fact.RepoPrefix == "" || fact.Owner == "" {
			return nil, fmt.Errorf("%w: ownership fact with no repository or owner",
				ErrDependencyRevisionIncomplete)
		}
		if _, ok := members[fact.RepoPrefix]; !ok {
			return nil, fmt.Errorf("%w: ownership names repository %q, which is not a roster member",
				ErrDependencyRevisionIncomplete, fact.RepoPrefix)
		}
		key := ownershipKey{fact.RepoPrefix, fact.Language, fact.Scope}
		if existing, ok := owners[key]; ok {
			if existing != fact.Owner {
				return nil, fmt.Errorf("%w: ownership of %q/%q in %q is both %q and %q",
					ErrDependencyRevisionIncomplete, fact.Language, fact.Scope, fact.RepoPrefix,
					existing, fact.Owner)
			}
			continue
		}
		owners[key] = fact.Owner
		deduped = append(deduped, fact)
	}
	return deduped, nil
}

// canonicalDependencyRevisionConfigSections validates and sorts the named
// configuration digests.
func canonicalDependencyRevisionConfigSections(
	sections []DependencyRevisionConfigSection,
) ([]DependencyRevisionConfigSection, error) {
	out := slices.Clone(sections)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Digest < out[j].Digest
	})
	seen := make(map[string]struct{}, len(out))
	for _, section := range out {
		if section.Name == "" || section.Digest == "" {
			return nil, fmt.Errorf("%w: configuration section with no name or digest",
				ErrDependencyRevisionIncomplete)
		}
		if _, duplicate := seen[section.Name]; duplicate {
			return nil, fmt.Errorf("%w: configuration section %q listed twice",
				ErrDependencyRevisionIncomplete, section.Name)
		}
		seen[section.Name] = struct{}{}
	}
	for _, domain := range dependencyRevisionRequiredConfigDomains {
		if _, ok := seen[domain]; !ok {
			return nil, fmt.Errorf("%w: configuration domain %q is not described",
				ErrDependencyRevisionIncomplete, domain)
		}
	}
	return out, nil
}

// canonicalDependencyRevisionProducers validates and sorts the producer policy.
// An empty policy is refused: silence about producers is itself a claim (see
// declareProducers), and a cohort that makes no claim cannot certify one.
func canonicalDependencyRevisionProducers(
	producers []DependencyRevisionProducer,
) ([]DependencyRevisionProducer, error) {
	if len(producers) == 0 {
		return nil, fmt.Errorf("%w: no producer policy", ErrDependencyRevisionIncomplete)
	}
	out := slices.Clone(producers)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Producer != out[j].Producer {
			return out[i].Producer < out[j].Producer
		}
		if out[i].State != out[j].State {
			return out[i].State < out[j].State
		}
		return out[i].Reason < out[j].Reason
	})
	seen := make(map[string]struct{}, len(out))
	for _, producer := range out {
		if producer.Producer == "" || producer.State == "" {
			return nil, fmt.Errorf("%w: producer policy row with no producer or state",
				ErrDependencyRevisionIncomplete)
		}
		if _, duplicate := seen[producer.Producer]; duplicate {
			return nil, fmt.Errorf("%w: producer %q declared twice",
				ErrDependencyRevisionIncomplete, producer.Producer)
		}
		seen[producer.Producer] = struct{}{}
	}
	return out, nil
}

// canonicalDependencyRevisionCapabilities validates, sorts and de-duplicates
// the capability vocabulary. Duplicates in a set carry no information, so they
// collapse; an id outside the closed capability set is refused rather than
// digested, because a cohort naming a capability the system does not have is
// describing something other than what will be built.
func canonicalDependencyRevisionCapabilities(capabilities []string) ([]string, error) {
	if len(capabilities) == 0 {
		return nil, fmt.Errorf("%w: no capability set", ErrDependencyRevisionIncomplete)
	}
	out := slices.Clone(capabilities)
	slices.Sort(out)
	out = slices.Compact(out)
	for _, capability := range out {
		if !graphview.CapabilityID(capability).Valid() {
			return nil, fmt.Errorf("%w: %q is not a defined capability",
				ErrDependencyRevisionIncomplete, capability)
		}
	}
	return out, nil
}

// dependencyRevisionEncoder writes the canonical length-delimited encoding.
//
// It is the shape generationIdentityKey already uses for the revision it
// carries (`<label>:<len>:<value>`, NUL-terminated): the length prefix makes
// the encoding injective, so no value containing a colon, a NUL or a label can
// be read as a different cohort's fields. Labels are compile-time literals.
type dependencyRevisionEncoder struct{ b strings.Builder }

func (e *dependencyRevisionEncoder) field(label, value string) {
	e.b.WriteString(label)
	e.b.WriteByte(':')
	e.b.WriteString(strconv.Itoa(len(value)))
	e.b.WriteByte(':')
	e.b.WriteString(value)
	e.b.WriteByte(0)
}

// count writes a list header so a list's length is part of the encoding rather
// than something a reader has to infer from the following fields.
func (e *dependencyRevisionEncoder) count(label string, n int) {
	e.field(label+".count", strconv.Itoa(n))
}

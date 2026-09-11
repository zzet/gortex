package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
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
// while a repository the resolver can actually see is missing from it.
//
// What a caller must NOT do with the error is leave
// `GenerationIdentity.DependencyRevision` empty: empty is the LEGACY value,
// which `generationIdentityKey` renders byte-for-byte as the pre-cohort key
// that every stored generation matches, so two mutually undescribable cohorts
// would share one identity. A caller that must build anyway stamps
// `dependencyCohortSource.degradedRevision` instead — outside this vocabulary,
// so nothing certified is ever reused for it, and stable, so a transient costs
// at most one rebuild rather than one per cycle. A caller that can wait should
// defer the build until the cohort is describable; see
// `CheckoutCoordinator.cohortAllowsBuild` for the bounded form of that policy.
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

	// RosterScope names what "complete" means for this cohort, in the
	// DependencyRevisionScope* vocabulary.
	//
	// Empty is the original claim: Repositories is every registered repository
	// in the daemon. A non-empty scope narrows that claim to the repositories
	// the target's resolution may actually consult — a workspace, or the target
	// repository alone when no workspace topology is available.
	//
	// It is digested, so a cohort that enumerated one workspace and a cohort
	// that enumerated the whole daemon are different claims even when they list
	// the same repositories. Without it the certificate would be ambiguous: a
	// reader comparing two revisions could not tell a complete roster from a
	// scoped one that happened to match.
	RosterScope string

	// RosterComplete is the caller's assertion that Repositories is the
	// complete roster FOR THE DECLARED SCOPE — the one it just validated under
	// the lease that produced it, not a filtered or cached subset. False
	// refuses.
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
	return DependencyRevisionRosterScoped(lease, nil, sourceIdentity)
}

// DependencyRevisionRosterScoped is DependencyRevisionRoster narrowed to the
// repositories a scope admits.
//
// inScope reports whether one registered repository belongs in this cohort;
// nil admits every one of them, which is what DependencyRevisionRoster asks
// for. The narrowing is NOT a relaxation of the completeness rule: the lease is
// still re-validated against the whole live registration set first, so a roster
// that gained or lost ANY repository under the digest is refused here, and
// every admitted member still has to name its bytes. What the scope decides is
// which repositories are inputs at all — a repository the target's resolution
// can never consult is not an input, and digesting it would make a commit in an
// unrelated repository re-key a payload it cannot change.
//
// The caller must declare the scope it used in
// DependencyRevisionInputs.RosterScope; the two together are the claim.
func DependencyRevisionRosterScoped(
	lease *graphview.RepositoryRosterLease,
	inScope func(repoPrefix string) bool,
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
	admits := func(repoPrefix string) bool { return inScope == nil || inScope(repoPrefix) }
	out := make([]DependencyRevisionRepository, 0, len(dedicated)+len(raw))
	for _, owner := range dedicated {
		if !admits(owner.RepoPrefix) {
			continue
		}
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
		if !admits(owner.RepoPrefix) {
			continue
		}
		out = append(out, DependencyRevisionRepository{
			RepoPrefix:     owner.RepoPrefix,
			Kind:           DependencyRevisionRepositoryRaw,
			Incarnation:    owner.Incarnation,
			RootIdentity:   owner.RootIdentity,
			SourceIdentity: sourceIdentity(owner.RepoPrefix),
		})
	}
	if len(out) == 0 {
		if inScope != nil {
			return nil, fmt.Errorf(
				"%w: the roster lease carries no repository this cohort's scope admits",
				ErrDependencyRevisionIncomplete)
		}
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
	e.field("roster.scope", inputs.RosterScope)

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

// --- cohort scope ---------------------------------------------------------

// The scope vocabulary a cohort declares. A revision is a certificate over the
// inputs its scope names, so the scope is digested with them: a cohort that
// enumerated one workspace and a cohort that enumerated the whole daemon are
// different claims even when they happen to list the same repositories.
const (
	// DependencyRevisionScopeWorkspace prefixes the scope of a cohort that
	// enumerated the repositories sharing the target's workspace.
	DependencyRevisionScopeWorkspace = "workspace:"
	// DependencyRevisionScopeRepository prefixes the scope of a cohort that
	// could name no workspace topology and enumerated the target repository
	// alone.
	DependencyRevisionScopeRepository = "repository:"
)

// DependencyRevisionDegradedPrefix marks a revision that is NOT a certificate:
// the cohort could not be described, and the value carries the stable reason
// instead.
//
// It is deliberately outside the DependencyRevisionEncodingVersion vocabulary,
// so no reader can mistake one for the other, and deliberately STABLE rather
// than unique: a value minted per coordinator or per call can never be matched
// again, so every layer stamped with one is permanently unreusable and rebuilds
// on every cycle for as long as the transient lasts. A stable value costs at
// most the reuse of one degraded layer by another build whose describable
// inputs are identical, and re-keys normally the moment the cohort can be
// described again.
const DependencyRevisionDegradedPrefix = "cohort-degraded"

// Stable refusal reasons. The vocabulary is closed: the reason rides in a
// stored identity, so an unbounded reason string would be an unbounded key
// space, and an error message reworded in another package would silently
// re-key every degraded layer in the field.
const (
	dependencyCohortReasonNoLeases    = "no-lease-manager"
	dependencyCohortReasonClosing     = "repository-closing"
	dependencyCohortReasonRawPending  = "raw-repository-provisional"
	dependencyCohortReasonStopped     = "admissions-stopped"
	dependencyCohortReasonRosterMoved = "roster-moved"
	dependencyCohortReasonIncomplete  = "cohort-incomplete"
	dependencyCohortReasonUnavailable = "cohort-unavailable"
)

// dependencyCohortRefusalReason classifies a refusal into the closed reason
// vocabulary above. Unrecognised causes fall to "cohort-unavailable" rather
// than to the error's own text.
func dependencyCohortRefusalReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, graphview.ErrRepositoryAdmissionsStopped):
		return dependencyCohortReasonStopped
	case errors.Is(err, graphview.ErrRepositoryAdmissionClosed):
		return dependencyCohortReasonClosing
	case errors.Is(err, graphview.ErrRawRepositoryNotReady):
		return dependencyCohortReasonRawPending
	case errors.Is(err, graphview.ErrRepositoryOwnerConflict):
		return dependencyCohortReasonRosterMoved
	case errors.Is(err, ErrDependencyRevisionIncomplete):
		return dependencyCohortReasonIncomplete
	default:
		return dependencyCohortReasonUnavailable
	}
}

// dependencyCohortCounters records what describing one cohort cost.
//
// It exists so a test can assert the COST rather than infer it: an idle poll
// must take no roster lease and read no roster member's source, and the only
// honest way to state that is to count the two operations where they happen.
type dependencyCohortCounters struct {
	RosterAcquisitions atomic.Int64
	MemberSourceReads  atomic.Int64
}

// dependencyCohortSource is everything one cohort digest needs from its caller.
//
// It is a value rather than a method set on the coordinator because two
// producers need the same digest over the same rules — a checkout's commit and
// working-tree layers, and a ref view's layer — and a second copy of the
// enumeration is a second place for the two to drift.
type dependencyCohortSource struct {
	// Target is the repository, workspace and project the generation is for.
	Target DependencyRevisionTarget

	// Leases enumerates the registered roster. nil refuses the cohort: a
	// producer that cannot see the roster cannot certify it.
	Leases *graphview.LeaseManager
	// Catalog names a dedicated roster member's committed corpus.
	Catalog *store_sqlite.Catalog

	// WorkspaceMembers reports the repositories that share the target's
	// workspace — MultiIndexer.ReposInWorkspace in production, which is the
	// established authority on "the complete list of repos a workspace-scoped
	// session is permitted to see". nil, or an answer that does not contain
	// the target, narrows the cohort to the target repository alone and says
	// so in the declared scope.
	WorkspaceMembers func() map[string]bool

	Config            config.IndexConfig
	ConfigSections    []DependencyRevisionConfigSection
	Ownership         []DependencyRevisionOwnership
	Producers         []DependencyRevisionProducer
	Capabilities      []string
	ExtractorVersions string

	// SourceBudget bounds how long naming one raw member's source may take.
	SourceBudget time.Duration

	// Counters is optional accounting; nil counts nothing.
	Counters *dependencyCohortCounters
}

// scope decides which repositories this cohort may name, and what to call the
// decision.
//
// A checkout layer's payload is produced by a build whose resolver runs inside
// ONE repository (`SparseGenerationBuilder.runPass` constructs a private
// Indexer with the MultiIndexer link deliberately unset, and `declareProducers`
// declares CapResolutionCrossRepo incomplete for exactly that reason), and
// cross-repository resolution is a MultiIndexer pass that no sparse build
// reaches. What a workspace sibling contains therefore cannot change this
// payload today.
//
// The cohort keeps workspace siblings anyway — conservatively, because
// workspace-scoped resolution is the boundary the rest of the daemon treats as
// "what this session may see", and a cohort that named less than the boundary
// would have to be re-derived the moment cross-repository binding becomes
// reachable from a sparse build. What it must NOT do is name repositories
// OUTSIDE the workspace: those can never be consulted, and digesting their
// tree OIDs makes a commit in any tracked repository re-key and rebuild every
// checkout layer in the daemon — the write amplification this whole change
// exists to remove.
func (s dependencyCohortSource) scope() (string, func(string) bool) {
	if s.WorkspaceMembers != nil && s.Target.WorkspaceID != "" {
		if members := s.WorkspaceMembers(); members[s.Target.RepoPrefix] {
			return DependencyRevisionScopeWorkspace + s.Target.WorkspaceID,
				func(prefix string) bool { return members[prefix] }
		}
	}
	return DependencyRevisionScopeRepository + s.Target.RepoPrefix,
		func(prefix string) bool { return prefix == s.Target.RepoPrefix }
}

// topologyToken is the cheap observation a poll re-checks to decide whether
// the cached cohort can still be trusted.
//
// It reads ONLY the workspace topology — MultiIndexer.ReposInWorkspace, a map
// walk under that struct's own read lock. No roster lease, no catalog read, no
// source witness: it is affordable on every poll of every checkout, which is
// the whole reason the cohort itself is not described there.
//
// What it therefore detects is a change in the cohort's MEMBERSHIP — a
// repository tracked into or untracked out of this checkout's workspace, which
// is the event that changes which repositories are inputs at all. What it
// cannot detect is a member's bytes moving; that reaches the cache through
// InvalidateDependencyCohort, and every build path describes the cohort afresh
// regardless, so no layer is ever STAMPED with a token-aged revision.
func (s dependencyCohortSource) topologyToken() string {
	var e dependencyRevisionEncoder
	if s.WorkspaceMembers == nil || s.Target.WorkspaceID == "" {
		e.field("scope", DependencyRevisionScopeRepository+s.Target.RepoPrefix)
		return e.b.String()
	}
	members := s.WorkspaceMembers()
	if !members[s.Target.RepoPrefix] {
		e.field("scope", DependencyRevisionScopeRepository+s.Target.RepoPrefix)
		return e.b.String()
	}
	names := make([]string, 0, len(members))
	for prefix, in := range members {
		if in {
			names = append(names, prefix)
		}
	}
	sort.Strings(names)
	e.field("scope", DependencyRevisionScopeWorkspace+s.Target.WorkspaceID)
	e.count("members", len(names))
	for _, name := range names {
		e.field("member", name)
	}
	return e.b.String()
}

// revision describes the cohort and digests it, or refuses.
//
// The roster lease is held across its own validation, the enumeration of every
// in-scope member's source and the digest — the window DependencyRevisionRoster
// documents. It is released before the caller's build and its catalog writes:
// a checkout build indexes a tree, and holding the roster lease across it would
// block every repository registration and every admission close for the length
// of a build, which lifecycle teardown would then wait on. What the shorter
// window costs is bounded by the rule that a changed revision ROOTS a new
// chain rather than extending one.
func (s dependencyCohortSource) revision(ctx context.Context) (string, error) {
	inputs, err := s.describe(ctx)
	if err != nil {
		return "", err
	}
	return ComputeDependencyRevision(inputs)
}

// describe assembles the cohort without digesting it, so a test can read the
// real production cohort and move exactly one dimension of it.
func (s dependencyCohortSource) describe(ctx context.Context) (DependencyRevisionInputs, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.Leases == nil {
		return DependencyRevisionInputs{}, fmt.Errorf(
			"%w: no lease manager to enumerate the roster with", ErrDependencyRevisionIncomplete)
	}
	// The scope is resolved BEFORE the roster lease is taken, deliberately.
	// WorkspaceMembers reads the repository topology under the MultiIndexer's
	// own lock; taking it while holding a roster lease would nest the two in
	// one order while a caller that holds the topology lock and then closes a
	// repository admission nests them in the other.
	label, member := s.scope()
	if s.Counters != nil {
		s.Counters.RosterAcquisitions.Add(1)
	}
	lease, err := s.Leases.AcquireRepositoryRoster()
	if err != nil {
		return DependencyRevisionInputs{}, fmt.Errorf("%w: acquire the repository roster: %w",
			ErrDependencyRevisionIncomplete, err)
	}
	defer lease.Release()
	return s.scopedInputs(ctx, lease, label, member)
}

// inputs assembles the cohort against a roster lease the caller holds.
func (s dependencyCohortSource) inputs(
	ctx context.Context, lease *graphview.RepositoryRosterLease,
) (DependencyRevisionInputs, error) {
	label, member := s.scope()
	return s.scopedInputs(ctx, lease, label, member)
}

// scopedInputs is inputs with the scope already decided, so the caller can
// resolve it before taking the roster lease.
func (s dependencyCohortSource) scopedInputs(
	ctx context.Context, lease *graphview.RepositoryRosterLease,
	label string, member func(string) bool,
) (DependencyRevisionInputs, error) {
	sources, err := s.sourceIdentities(ctx, lease, member)
	if err != nil {
		return DependencyRevisionInputs{}, err
	}
	repositories, err := DependencyRevisionRosterScoped(lease, member, func(repoPrefix string) string {
		return sources[repoPrefix]
	})
	if err != nil {
		return DependencyRevisionInputs{}, err
	}
	return DependencyRevisionInputs{
		Target:            s.Target,
		RosterScope:       label,
		RosterComplete:    true,
		Repositories:      repositories,
		OwnershipComplete: true,
		Ownership:         s.Ownership,
		Config:            s.Config,
		ConfigSections:    s.ConfigSections,
		Producers:         s.Producers,
		Capabilities:      s.Capabilities,
		ExtractorVersions: s.ExtractorVersions,
	}, nil
}

// sourceIdentities names what this build can see of each IN-SCOPE roster
// member's bytes.
//
// A dedicated repository's source is the committed corpus its graph is at — the
// same primary base every layer over that graph is built against, so the cohort
// and the layer cannot disagree about what the corpus is. A raw repository has
// no committed tree; its source witness (revision plus content fingerprint) is
// the equivalent, read under a bounded budget so a concurrent source mutation
// cannot park the caller.
//
// Every refusal is a refusal of the whole cohort — a member whose bytes cannot
// be named is exactly the false certificate a revision must not issue — but
// only for members the scope admits. An out-of-scope repository is not read at
// all: not its catalog row, not its source witness. That is both the
// correctness fix (its bytes are not an input) and the cost fix (the per-cycle
// read is bounded by the workspace, not by the daemon).
func (s dependencyCohortSource) sourceIdentities(
	ctx context.Context, lease *graphview.RepositoryRosterLease, member func(string) bool,
) (map[string]string, error) {
	out := map[string]string{}
	if s.Catalog == nil {
		return nil, fmt.Errorf("%w: no catalog to name a roster member's corpus with",
			ErrDependencyRevisionIncomplete)
	}
	budget := s.SourceBudget
	if budget <= 0 {
		budget = dependencyRevisionSourceBudget
	}
	for _, owner := range lease.DedicatedOwners() {
		if member != nil && !member(owner.RepoPrefix) {
			continue
		}
		if s.Counters != nil {
			s.Counters.MemberSourceReads.Add(1)
		}
		dedicated, found, err := s.Catalog.GetDedicatedGraph(ctx, owner.GraphID)
		if err != nil {
			return nil, fmt.Errorf("%w: read dedicated graph %s: %w",
				ErrDependencyRevisionIncomplete, owner.GraphID, err)
		}
		if !found {
			return nil, fmt.Errorf("%w: dedicated graph %s is not in the catalog",
				ErrDependencyRevisionIncomplete, owner.GraphID)
		}
		base, err := graphBase(ctx, s.Catalog, dedicated)
		if err != nil {
			return nil, fmt.Errorf("%w: name the corpus of %s: %w",
				ErrDependencyRevisionIncomplete, owner.RepoPrefix, err)
		}
		if base.treeOID == "" {
			return nil, fmt.Errorf("%w: repository %s names no committed tree",
				ErrDependencyRevisionIncomplete, owner.RepoPrefix)
		}
		out[owner.RepoPrefix] = "tree:" + base.treeOID
	}
	for _, registration := range lease.RawRegistrations() {
		owner := registration.Owner()
		if member != nil && !member(owner.RepoPrefix) {
			continue
		}
		if s.Counters != nil {
			s.Counters.MemberSourceReads.Add(1)
		}
		witnessCtx, cancel := context.WithTimeout(ctx, budget)
		snapshot, err := s.Leases.AcquireRawRepositorySnapshot(witnessCtx, registration, 0)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("%w: witness the source of raw repository %s: %w",
				ErrDependencyRevisionIncomplete, owner.RepoPrefix, err)
		}
		witness := snapshot.Witness()
		snapshot.Release()
		if witness.Revision == 0 || witness.Fingerprint == "" {
			return nil, fmt.Errorf("%w: raw repository %s has no source witness",
				ErrDependencyRevisionIncomplete, owner.RepoPrefix)
		}
		out[owner.RepoPrefix] = "raw:" + strconv.FormatUint(witness.Revision, 10) +
			":" + witness.Fingerprint
	}
	return out, nil
}

// degradedRevision is the value a producer stamps when the cohort cannot be
// described and the build must go ahead anyway.
//
// It is NOT a certificate and says so: the DependencyRevisionDegradedPrefix
// vocabulary can never equal a computed revision, so nothing built under a
// described cohort is ever reused for one that was not, in either direction.
// What it IS, is stable and content-derived — the reason plus everything the
// producer CAN name (the target, the configuration, the producer policy, the
// capability vocabulary and the extractor set). Two builds a minute apart under
// the same transient produce the same value, so a degraded layer is reusable by
// the next degraded build of the same inputs instead of rebuilding every cycle;
// and a change in any describable input still re-keys it.
//
// The residual is declared rather than hidden: what a degraded revision cannot
// see is the roster it failed to enumerate, so two degraded builds whose
// describable inputs match are treated as one even if the roster moved between
// them. That is bounded by the prefix — the moment the cohort is describable
// again the identity re-keys to a real revision — and by the caller's own
// policy of preferring to DEFER a build over stamping one of these.
func (s dependencyCohortSource) degradedRevision(reason string) string {
	if reason == "" {
		reason = dependencyCohortReasonUnavailable
	}
	scope, _ := s.scope()
	configDigest := "unencodable-configuration"
	if _, digest, err := snapshotDedicatedBaseConfig(
		s.Config, s.Target.RepoPrefix, s.Target.WorkspaceID, s.Target.ProjectID); err == nil {
		configDigest = digest
	}
	var e dependencyRevisionEncoder
	e.field("encoding", DependencyRevisionDegradedPrefix)
	e.field("reason", reason)
	e.field("scope", scope)
	e.field("target.repo", s.Target.RepoPrefix)
	e.field("target.workspace", s.Target.WorkspaceID)
	e.field("target.project", s.Target.ProjectID)
	e.field("config.index", configDigest)
	e.count("config.sections", len(s.ConfigSections))
	for _, section := range sortedConfigSections(s.ConfigSections) {
		e.field("config.section.name", section.Name)
		e.field("config.section.digest", section.Digest)
	}
	e.count("producers", len(s.Producers))
	for _, producer := range sortedProducers(s.Producers) {
		e.field("producer.id", producer.Producer)
		e.field("producer.state", producer.State)
		e.field("producer.reason", producer.Reason)
	}
	e.count("capabilities", len(s.Capabilities))
	for _, capability := range slices.Sorted(slices.Values(s.Capabilities)) {
		e.field("capability", capability)
	}
	e.field("extractors", s.ExtractorVersions)
	sum := sha256.Sum256([]byte(e.b.String()))
	return DependencyRevisionDegradedPrefix + ":" + reason + ":" + hex.EncodeToString(sum[:16])
}

// sortedConfigSections and sortedProducers order the degraded digest's lists
// the way the certified encoding orders them, so the two agree about what
// "the same inputs" means.
func sortedConfigSections(in []DependencyRevisionConfigSection) []DependencyRevisionConfigSection {
	out := slices.Clone(in)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Digest < out[j].Digest
	})
	return out
}

func sortedProducers(in []DependencyRevisionProducer) []DependencyRevisionProducer {
	out := slices.Clone(in)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Producer != out[j].Producer {
			return out[i].Producer < out[j].Producer
		}
		if out[i].State != out[j].State {
			return out[i].State < out[j].State
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

package resolver

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// The resolution contract's version, as a value a cache key can carry.
//
// A generation's payload is a function of what resolution emits over its file
// set. The catalog stores that dependence as one column, `resolver_version`,
// and every reuse guard compares it: a stored generation whose resolver
// version is not this binary's is not what this binary would produce, so it is
// refused rather than served.
//
// Until this file existed the column was the compile-time literal "1" in two
// places (`indexer.checkoutResolverVersion`, `indexer.refViewResolverVersion`).
// A literal only invalidates when a human remembers to raise it, and the two
// copies could disagree; what a reuse guard needs instead is a value that
// MOVES when the contract moves. Version() derives one.

// ResolutionSemanticsVersion names the resolution contract's semantics —
// everything about what resolution emits that the registries below do not
// name: the binding cascade, the candidate gates, the confidence tiers, the
// restub and incoming-edge rules, and the internals of any single pass.
//
// It is hand maintained ON PURPOSE. A fingerprint over the registered pass
// names catches a pass that joins, leaves or is renamed; it cannot catch an
// edit INSIDE a pass, and no cheap derivation can. Raise it in the same change
// that alters what resolution emits for an unchanged corpus, exactly as an
// extractor's policy version is raised (`indexer.extractorVersionsSnapshot`).
//
// Raising it re-keys every stored generation once: nothing an older binary
// published matches, every layer rebuilds on first contact, and from then on
// the cache is keyed by the contract that produced it.
const ResolutionSemanticsVersion = 1

// resolverVersionDomain versions the ENCODING below, not the contract. Two
// fingerprints that disagree about how the preimage is rendered must not be
// comparable, so the domain is digested with everything else and the version
// string carries the semantics version in clear text.
const resolverVersionDomain = "gortex.resolver.contract.v1"

// Version renders the resolution contract this binary implements as one
// comparable string, in the shape the other identity columns use:
// `resolver-<semantics>:<digest>`.
//
// The digest covers the registered pass set — every framework synthesizer and
// every claiming resolver, in canonical execution order
// (`FrameworkSynthesizerNames`) — plus the hand-maintained semantics version.
// Order is digested rather than sorted away because it is load-bearing: the
// synthesizers run in a fixed sequence and some read the edges an earlier one
// landed (`defaultFrameworkSynthesizers`), so two registries with the same
// members in a different order do not emit the same graph.
//
// What it does NOT cover is stated plainly so no reader over-trusts it: a
// change inside a pass's body, a new binding rule in the cascade, a changed
// confidence tier. Those move ResolutionSemanticsVersion, which is digested
// here, and a golden test pins the produced value so an unaccompanied drift in
// either half fails the build rather than silently serving stale payload.
func Version() string {
	return resolverVersionFingerprint(FrameworkSynthesizerNames(), ResolutionSemanticsVersion)
}

// resolverVersionFingerprint is the pure half: the encoding, over inputs a
// test can supply. Every value is length-delimited, so no pass name can
// imitate a delimiter and make two distinct registries collide.
func resolverVersionFingerprint(passes []string, semantics int) string {
	var b strings.Builder
	field := func(label, value string) {
		b.WriteString(label)
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(len(value)))
		b.WriteByte(':')
		b.WriteString(value)
		b.WriteByte(0)
	}
	field("domain", resolverVersionDomain)
	field("semantics", strconv.Itoa(semantics))
	field("passes", strconv.Itoa(len(passes)))
	for _, pass := range passes {
		field("pass", pass)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "resolver-" + strconv.Itoa(semantics) + ":" + hex.EncodeToString(sum[:16])
}

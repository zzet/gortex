package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// C# `using static Ns.Cls;` — and its compilation-scoped `global using
// static` sibling — brings Cls's STATIC members into scope by simple name,
// so a receiverless `Foo(...)` may be `Cls.Foo`. The extension binder
// already reads the same directive for extension form (csharp_extension.go);
// this file is the receiverless form the ladder in resolveFunctionCall had
// no tier for: the call bound wherever directory locality pointed, because
// that is the only evidence the generic cascade knows.
//
// Simple-name lookup order (C# spec §12.8.4): members of the enclosing type
// and its bases first; then, per enclosing namespace from inner to outer,
// the namespace's own members and its using-static imports. The rule
// therefore runs only after the enclosing chain has declined the name
// (csharpEnclosingTypeClaims), and it is narrowing only: a candidate set no
// using-static target owns falls through to the cascade unchanged.
//
// What the directive does NOT import, and the tier refuses accordingly:
// instance members (reachable only through an instance — `using static`
// is legal on any type with static members) and extension methods (ECMA
// §14.6.4: made available for extension invocation, never as static
// methods; the extension binder owns those).
//
// The caller's OWN declaration space comes first of all: a local function,
// a delegate-typed parameter or local named like the call is the callee
// (spec §12.8.4), and none of those is a node the graph can point at. The
// extractor stamps such a call edge Meta["local_shadow"] at the call's
// offset (csharp.go, the receiverless branch; local functions join the
// scope index in csharp_binding_scopes.go), and csharpLocalShadowed keeps
// the stub unresolved before ANY tier runs — the same-file pick was
// binding those at 0.9 too. What remains unseen is a member inherited
// from an external base (`Ok(...)` in a controller), which the in-graph
// ancestor walk cannot know about: the directive's member wins there.
//
// Deliberately NOT done: qualifying a bare name the repo does not declare
// onto an external using-static target (`Sqrt` under `using static
// System.Math` → `Math.Sqrt`). With no in-repo node there is nothing to
// bind, and the external-base ambiguity above applies with no upside;
// that attribution needs a member model of the target or the LSP lane.
//
// Known limits, inherited from the visibility model the extension binder
// already uses: the statics set is file-flat (a `using static` written
// inside `namespace A` is visible to a sibling `namespace B` of the same
// file); a directive naming a NESTED static class (`using static
// Outer.Inner;`) never matches, because a member's owner FQN is
// scope_ns + direct owner and the graph records no outer-type chain — a
// silent miss, never a wrong bind; and for the same reason the tier sits
// behind the same-file pick, which is what answers a nested type's call to
// its enclosing type's static member when both share a file.

// csharpBindUsingStaticCall binds a receiverless C# call to the static
// member a visible using-static target declares. candidates is the
// applicability-narrowed same-name set. Returns true when it bound.
func (r *Resolver) csharpBindUsingStaticCall(e *graph.Edge, name string, candidates []*graph.Node, stats *ResolveStats) bool {
	if len(candidates) == 0 || !strings.HasSuffix(e.FilePath, ".cs") {
		return false
	}
	visible := r.csharpFileNamespaceSet(e.FilePath)
	if len(visible.statics) == 0 {
		return false
	}
	// Owned candidates first, the enclosing-chain walk only when there is
	// something to decline: on a unit with a repo-wide global static every
	// receiverless call passes through here.
	var picks []*graph.Node
	owner := ""
	for _, c := range candidates {
		if !csharpUsingStaticImportable(c) {
			continue
		}
		fqn, ok := csharpUsingStaticOwner(visible, c)
		if !ok {
			continue
		}
		if owner != "" && fqn != owner {
			// Two targets both declare the name — the compiler would
			// report an ambiguity; nothing here picks one.
			return false
		}
		owner = fqn
		picks = append(picks, c)
	}
	if len(picks) == 0 {
		return false
	}
	if r.csharpEnclosingTypeClaims(e, name) {
		return false
	}
	e.To = picks[0].ID
	// The owner is proven by the directive; a lone survivor of the
	// applicability window is the member. Several same-owner overloads
	// surviving make the member a guess inside a proven owner: the first
	// binds at the inferred tier, and csharpUsingStaticGuardKeep is what
	// carries that weak-tier bind past the cross-package guard.
	if len(picks) == 1 {
		e.Origin = graph.OriginASTResolved
		e.Confidence = 0.9
	} else {
		e.Origin = graph.OriginASTInferred
		e.Confidence = 0.75
	}
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	e.Meta["resolution"] = "using_static"
	stats.Resolved++
	return true
}

// csharpUsingStaticImportable reports whether a candidate is a member a
// `using static` directive can bring into scope by simple name: a C#
// method carrying the extractor's static stamp that is not an extension
// method. The stamp is written only when true, so an instance method (or
// a pre-stamp graph) simply never qualifies — a miss, never a wrong bind.
func csharpUsingStaticImportable(c *graph.Node) bool {
	if c == nil || c.Kind != graph.KindMethod || c.Meta == nil || !sameLanguageFamily("csharp", c.Language) {
		return false
	}
	if static, _ := c.Meta["static"].(bool); !static {
		return false
	}
	return !isCSharpExtension(c)
}

// csharpLocalShadowed reports whether the C# extractor stamped this
// receiverless call as bound by the caller's own declaration space — a
// local function, a delegate-typed parameter or local of the same simple
// name. Such a call has no in-graph callee; every tier must leave the
// stub alone.
func csharpLocalShadowed(e *graph.Edge) bool {
	if e == nil || e.Meta == nil {
		return false
	}
	shadow, _ := e.Meta["local_shadow"].(bool)
	return shadow
}

// csharpDropStaleUsingStaticTag removes a using_static resolution tag
// from an edge about to be re-resolved. The incremental restub keeps
// Meta, so the tag would otherwise ride whatever the cascade binds next
// when the directive no longer applies — the hygiene the extension_method
// tag gets in resolveFunctionCall.
func csharpDropStaleUsingStaticTag(e *graph.Edge) {
	if e == nil || e.Meta == nil {
		return
	}
	if res, _ := e.Meta["resolution"].(string); res == "using_static" {
		delete(e.Meta, "resolution")
	}
}

// csharpUsingStaticGuardKeep is the cross-package guard's keep-rule for
// using-static binds: the directive, not an import of the target's
// directory, is what puts the member in scope, so a weak-tier bind whose
// declaring class is still a visible using-static target of the calling
// file must survive the import-reachability revert. Same evidence the
// bind used, so the two can never disagree.
func (r *Resolver) csharpUsingStaticGuardKeep(e *graph.Edge, callerFile string, target *graph.Node) bool {
	if e == nil || e.Meta == nil || callerFile == "" || !csharpUsingStaticImportable(target) {
		return false
	}
	if res, _ := e.Meta["resolution"].(string); res != "using_static" {
		return false
	}
	_, ok := csharpUsingStaticOwner(r.csharpFileNamespaceSet(callerFile), target)
	return ok
}

// csharpEnclosingTypeClaims reports whether the caller's own type or one
// of its ancestors declares a member with the call's name — the lookup
// tier that precedes every using-static import. In-graph proof only, the
// same stance as csharpInstanceMemberClaims: an external base that might
// declare the member changes nothing here.
func (r *Resolver) csharpEnclosingTypeClaims(e *graph.Edge, name string) bool {
	caller := r.cachedGetNode(e.From)
	if caller == nil {
		return false
	}
	recv := nodeReceiverType(caller)
	if recv == "" {
		return false
	}
	return r.csharpInstanceMemberClaims(e, recv, name)
}

// csharpUsingStaticOwner returns the using-static target FQN that owns
// candidate c — its declaring class (scope_ns + Meta["receiver"]) — or
// false when no visible target does. A directive written inside a
// namespace block may spell the target relative to that namespace
// (`namespace App { using static Util.H; }` for App.Util.H), so a
// dot-boundary suffix match counts too — anchored: the namespace the
// suffix leaves over must be one the calling file encloses, or a
// top-level `using static Helpers;` would reach every class whose FQN
// ends in ".Helpers" (the key-match-is-not-eligibility rule of
// csharpExtTypeKey).
func csharpUsingStaticOwner(visible csharpFileNS, c *graph.Node) (string, bool) {
	cls, _ := c.Meta["receiver"].(string)
	if cls == "" {
		return "", false
	}
	fqn := cls
	if ns, _ := c.Meta["scope_ns"].(string); ns != "" {
		fqn = ns + "." + cls
	}
	if _, ok := visible.statics[fqn]; ok {
		return fqn, true
	}
	for s := range visible.statics {
		if !strings.HasSuffix(fqn, "."+s) {
			continue
		}
		if _, enclosing := visible.enclosing[fqn[:len(fqn)-len(s)-1]]; enclosing {
			return fqn, true
		}
	}
	return "", false
}

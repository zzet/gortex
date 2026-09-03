package graphview

import (
	"fmt"
	"slices"
	"strings"
)

// SelectorKind is how a caller names the view it wants.
type SelectorKind string

const (
	// SelectorAuto lets the server pick: the caller's workspace default.
	SelectorAuto SelectorKind = "auto"
	// SelectorBase pins a persisted base graph by id.
	SelectorBase SelectorKind = "base"
	// SelectorWorktree pins a registered checkout by id, including its
	// working-tree edits.
	SelectorWorktree SelectorKind = "worktree"
	// SelectorGitRef pins the commit a full ref points at.
	SelectorGitRef SelectorKind = "git_ref"
	// SelectorCommit pins one commit by object id.
	SelectorCommit SelectorKind = "commit"
)

// Selector is a validated request for a view. Exactly which fields carry
// meaning depends on Kind; ParseSelector rejects any field the kind does not
// use, so a Selector value never carries a field that will be silently ignored.
type Selector struct {
	Kind       SelectorKind `json:"kind"`
	GraphID    string       `json:"graph_id,omitempty"`
	CheckoutID string       `json:"checkout_id,omitempty"`
	Value      string       `json:"value,omitempty"`
}

// ParseSelector validates a raw selector from the wire.
//
// An empty kind means auto: a caller that omits the field asks for the default
// view. Every other kind must be one of the SelectorKind constants.
//
//   - auto     — takes no other field.
//   - base     — requires graphID.
//   - worktree — requires checkoutID.
//   - git_ref  — requires value: a FULL ref name under refs/heads/,
//     refs/tags/, or refs/remotes/. Short names ("main"), HEAD, and revision
//     expressions ("main~1", "a..b", "x@{1}") are rejected: they resolve
//     against ambient state, and a pinned view may not depend on ambient state.
//     graphID is optional and names the graph whose corpus the ref composes
//     over; omitting it leaves the choice to the server, which has to be able
//     to make it unambiguously.
//   - commit   — requires value: a full lowercase hex object id, 40 (SHA-1) or
//     64 (SHA-256) characters. Abbreviated ids are ambiguous and rejected.
//     graphID is optional, exactly as for git_ref.
//
// A missing required field or a malformed value fails with
// CodeInvalidViewSelector; a field the kind does not use fails with
// CodeSelectorConflict. Values are never trimmed — leading or trailing
// whitespace in a ref name or an object id is a malformed value, not a typo to
// paper over.
func ParseSelector(kind, graphID, checkoutID, value string) (Selector, error) {
	k := SelectorKind(strings.TrimSpace(kind))
	if k == "" {
		k = SelectorAuto
	}
	s := Selector{Kind: k, GraphID: graphID, CheckoutID: checkoutID, Value: value}

	switch k {
	case SelectorAuto:
		if err := rejectUnused(s, ""); err != nil {
			return Selector{}, err
		}
	case SelectorBase:
		if graphID == "" {
			return Selector{}, NewViewError(CodeInvalidViewSelector, "base selector requires a graph id")
		}
		if err := rejectUnused(s, "graph_id"); err != nil {
			return Selector{}, err
		}
	case SelectorWorktree:
		if checkoutID == "" {
			return Selector{}, NewViewError(CodeInvalidViewSelector, "worktree selector requires a checkout id")
		}
		if err := rejectUnused(s, "checkout_id"); err != nil {
			return Selector{}, err
		}
	case SelectorGitRef:
		if err := rejectUnused(s, "graph_id", "value"); err != nil {
			return Selector{}, err
		}
		if err := validateFullRefName(value); err != nil {
			return Selector{}, err
		}
	case SelectorCommit:
		if err := rejectUnused(s, "graph_id", "value"); err != nil {
			return Selector{}, err
		}
		if err := validateCommitOID(value); err != nil {
			return Selector{}, err
		}
	default:
		return Selector{}, NewViewError(CodeInvalidViewSelector, fmt.Sprintf("unknown selector kind %q", string(k)))
	}
	return s, nil
}

// Equal reports whether two selectors ask for the same view.
func (s Selector) Equal(other Selector) bool { return s == other }

// String renders the selector in the "kind:value" form used in view riders and
// log lines. Auto has no payload, so it renders as the bare kind.
func (s Selector) String() string {
	switch s.Kind {
	case SelectorBase:
		return string(s.Kind) + ":" + s.GraphID
	case SelectorWorktree:
		return string(s.Kind) + ":" + s.CheckoutID
	case SelectorGitRef, SelectorCommit:
		if s.GraphID != "" {
			return string(s.Kind) + ":" + s.GraphID + ":" + s.Value
		}
		return string(s.Kind) + ":" + s.Value
	default:
		return string(s.Kind)
	}
}

// rejectUnused fails when a selector carries a field its kind does not use.
// used names the fields the kind consumes (none for auto).
func rejectUnused(s Selector, used ...string) error {
	fields := []struct {
		name  string
		value string
	}{
		{"graph_id", s.GraphID},
		{"checkout_id", s.CheckoutID},
		{"value", s.Value},
	}
	for _, f := range fields {
		if f.value == "" || slices.Contains(used, f.name) {
			continue
		}
		return NewViewError(CodeSelectorConflict,
			fmt.Sprintf("%s selector does not take %s", string(s.Kind), f.name))
	}
	return nil
}

// refNamespaces are the full-ref prefixes a view selector may name. Anything
// else (HEAD, refs/notes/, a bare name) is not a branch, tag, or remote branch.
var refNamespaces = []string{"refs/heads/", "refs/tags/", "refs/remotes/"}

// refBannedBytes are the bytes git-check-ref-format forbids outright. Space and
// the control range are checked separately.
const refBannedBytes = "~^:?*[\\"

// validateFullRefName applies the git-check-ref-format rules that matter for a
// full ref name, plus the namespace restriction above. It is deliberately
// stricter than git: only complete ref names pass, so the ref a view pins
// cannot change meaning with the caller's HEAD or remote configuration.
func validateFullRefName(ref string) error {
	if ref == "" {
		return NewViewError(CodeInvalidViewSelector, "git_ref selector requires a ref name")
	}
	var ns string
	for _, p := range refNamespaces {
		if strings.HasPrefix(ref, p) {
			ns = p
			break
		}
	}
	if ns == "" {
		return NewViewError(CodeInvalidViewSelector,
			fmt.Sprintf("ref %q must be a full ref name under refs/heads/, refs/tags/, or refs/remotes/", ref))
	}
	if strings.TrimPrefix(ref, ns) == "" {
		return NewViewError(CodeInvalidViewSelector, fmt.Sprintf("ref %q names no ref inside %s", ref, ns))
	}
	if strings.Contains(ref, "..") {
		return NewViewError(CodeInvalidViewSelector, fmt.Sprintf("ref %q contains \"..\"", ref))
	}
	if strings.Contains(ref, "@{") {
		return NewViewError(CodeInvalidViewSelector, fmt.Sprintf("ref %q contains \"@{\"", ref))
	}
	for i := 0; i < len(ref); i++ {
		b := ref[i]
		switch {
		case b < 0x20 || b == 0x7f:
			return NewViewError(CodeInvalidViewSelector, fmt.Sprintf("ref %q contains a control character", ref))
		case b == ' ':
			return NewViewError(CodeInvalidViewSelector, fmt.Sprintf("ref %q contains a space", ref))
		case strings.IndexByte(refBannedBytes, b) >= 0:
			return NewViewError(CodeInvalidViewSelector, fmt.Sprintf("ref %q contains %q", ref, string(b)))
		}
	}
	if strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") {
		return NewViewError(CodeInvalidViewSelector, fmt.Sprintf("ref %q starts or ends with \"/\"", ref))
	}
	for _, part := range strings.Split(ref, "/") {
		switch {
		case part == "":
			return NewViewError(CodeInvalidViewSelector, fmt.Sprintf("ref %q has an empty path component", ref))
		case part == "@":
			return NewViewError(CodeInvalidViewSelector, fmt.Sprintf("ref %q has a component that is a bare \"@\"", ref))
		case strings.HasPrefix(part, "."):
			return NewViewError(CodeInvalidViewSelector, fmt.Sprintf("ref %q has a component starting with \".\"", ref))
		case strings.HasSuffix(part, "."):
			return NewViewError(CodeInvalidViewSelector, fmt.Sprintf("ref %q has a component ending with \".\"", ref))
		case strings.HasSuffix(part, ".lock"):
			return NewViewError(CodeInvalidViewSelector, fmt.Sprintf("ref %q has a component ending with \".lock\"", ref))
		}
	}
	return nil
}

// validateCommitOID accepts only a full lowercase hex object id: 40 hex digits
// for SHA-1, 64 for SHA-256.
func validateCommitOID(oid string) error {
	if oid == "" {
		return NewViewError(CodeInvalidViewSelector, "commit selector requires an object id")
	}
	if len(oid) != 40 && len(oid) != 64 {
		return NewViewError(CodeInvalidViewSelector,
			fmt.Sprintf("commit id %q must be a full 40- or 64-character hex object id", oid))
	}
	for i := 0; i < len(oid); i++ {
		b := oid[i]
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return NewViewError(CodeInvalidViewSelector,
				fmt.Sprintf("commit id %q must be lowercase hexadecimal", oid))
		}
	}
	return nil
}

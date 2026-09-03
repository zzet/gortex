package graphview

import (
	"errors"
	"strings"
)

// Stable error codes. These strings travel verbatim in API payloads, so they
// are part of the wire contract: a client may switch on them. Never reword a
// code — add a new one instead.
const (
	// CodeInvalidViewSelector: the selector is malformed — an unknown kind,
	// a missing required field, a ref name git itself would reject, or an
	// object id that is not a full hex oid.
	CodeInvalidViewSelector = "invalid_view_selector"
	// CodeRefNotCommit: the selector resolved to an object that is not a
	// commit (a tree, a blob, or an annotated tag whose target is neither).
	CodeRefNotCommit = "ref_not_commit"
	// CodeSelectorConflict: the selector carries fields its kind does not
	// use, or two selectors in one request name different views of the same
	// repository.
	CodeSelectorConflict = "selector_conflict"
	// CodeSelectorOutOfScope: the selector names a repository or checkout
	// outside the caller's workspace scope.
	CodeSelectorOutOfScope = "selector_out_of_scope"
	// CodeRefNotAvailableLocally: the ref or object is well-formed but the
	// local object store does not have it (never fetched, or pruned).
	CodeRefNotAvailableLocally = "ref_not_available_locally"
	// CodeViewBuilding: the view exists but is still being built; the
	// response carries a build token and a retry hint.
	CodeViewBuilding = "view_building"
	// CodeViewReadOnly: the request would mutate a view that only supports
	// reads (a pinned commit view, for instance).
	CodeViewReadOnly = "view_read_only"
	// CodeCapabilityUnavailable: a required capability cannot be served by
	// this view at all — it is unavailable or disabled by configuration.
	CodeCapabilityUnavailable = "capability_unavailable"
	// CodeRequiredCapabilityIncomplete: a required capability exists but is
	// not finished — still building, or only partially populated.
	CodeRequiredCapabilityIncomplete = "required_capability_incomplete"
	// CodeCheckoutInaccessible: the checkout backing the view cannot be
	// read (unmounted, deleted, or permission denied).
	CodeCheckoutInaccessible = "checkout_inaccessible"
	// CodeNoPrimary: the workspace has no primary checkout to fall back on.
	CodeNoPrimary = "no_primary"
	// CodePrimaryNotReady: the primary checkout exists but has not been
	// indexed far enough to answer.
	CodePrimaryNotReady = "primary_not_ready"
	// CodeSourceObjectMissing: the source bytes a result points at are gone
	// from the object store or the working tree.
	CodeSourceObjectMissing = "source_object_missing"
)

// Sentinel errors, one per code. They exist so callers can write
// errors.Is(err, graphview.ErrViewBuilding) without reaching for the code
// string. Matching is by code, so any error this package builds with a given
// code matches that code's sentinel.
var (
	ErrInvalidViewSelector          = &ViewError{Code: CodeInvalidViewSelector, Message: "invalid view selector"}
	ErrRefNotCommit                 = &ViewError{Code: CodeRefNotCommit, Message: "ref does not resolve to a commit"}
	ErrSelectorConflict             = &ViewError{Code: CodeSelectorConflict, Message: "conflicting view selectors"}
	ErrSelectorOutOfScope           = &ViewError{Code: CodeSelectorOutOfScope, Message: "selector is outside the workspace scope"}
	ErrRefNotAvailableLocally       = &ViewError{Code: CodeRefNotAvailableLocally, Message: "ref is not available in the local object store"}
	ErrViewBuilding                 = &ViewError{Code: CodeViewBuilding, Message: "view is still building"}
	ErrViewReadOnly                 = &ViewError{Code: CodeViewReadOnly, Message: "view is read-only"}
	ErrCapabilityUnavailable        = &ViewError{Code: CodeCapabilityUnavailable, Message: "required capability is unavailable"}
	ErrRequiredCapabilityIncomplete = &ViewError{Code: CodeRequiredCapabilityIncomplete, Message: "required capability is incomplete"}
	ErrCheckoutInaccessible         = &ViewError{Code: CodeCheckoutInaccessible, Message: "checkout is inaccessible"}
	ErrNoPrimary                    = &ViewError{Code: CodeNoPrimary, Message: "workspace has no primary checkout"}
	ErrPrimaryNotReady              = &ViewError{Code: CodePrimaryNotReady, Message: "primary checkout is not ready"}
	ErrSourceObjectMissing          = &ViewError{Code: CodeSourceObjectMissing, Message: "source object is missing"}
)

// ErrorCodes returns every code this package can produce, in a stable order.
// A caller that renders a code table (docs, a client enum) reads it from here
// so the table cannot drift from the constants.
func ErrorCodes() []string {
	return []string{
		CodeInvalidViewSelector,
		CodeRefNotCommit,
		CodeSelectorConflict,
		CodeSelectorOutOfScope,
		CodeRefNotAvailableLocally,
		CodeViewBuilding,
		CodeViewReadOnly,
		CodeCapabilityUnavailable,
		CodeRequiredCapabilityIncomplete,
		CodeCheckoutInaccessible,
		CodeNoPrimary,
		CodePrimaryNotReady,
		CodeSourceObjectMissing,
	}
}

// Coded is implemented by every error this package returns. It exposes the
// stable code that API payloads carry verbatim.
type Coded interface {
	error
	// ErrorCode returns the stable wire code.
	ErrorCode() string
}

// ViewError is the base error type of this package. It pairs a stable code
// with a human-readable message and an optional wrapped cause. Its JSON shape
// is the error payload the server hands back.
type ViewError struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`

	cause error
}

// NewViewError builds an error carrying code and message.
func NewViewError(code, message string) *ViewError {
	return &ViewError{Code: code, Message: message}
}

// WrapViewError builds an error carrying code and message that unwraps to
// cause, so a lower-level failure (a git or filesystem error) stays
// inspectable behind the stable code.
func WrapViewError(code, message string, cause error) *ViewError {
	return &ViewError{Code: code, Message: message, cause: cause}
}

// Error renders "code: message: cause", skipping the parts that are empty.
func (e *ViewError) Error() string {
	if e == nil {
		return "<nil view error>"
	}
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.cause != nil {
		b.WriteString(": ")
		b.WriteString(e.cause.Error())
	}
	return b.String()
}

// ErrorCode implements Coded.
func (e *ViewError) ErrorCode() string {
	if e == nil {
		return ""
	}
	return e.Code
}

// Unwrap exposes the wrapped cause, if any.
func (e *ViewError) Unwrap() error { return e.cause }

// Is matches any *ViewError carrying the same code, which is what makes the
// package sentinels work for errors.Is.
func (e *ViewError) Is(target error) bool {
	if e == nil {
		return false
	}
	t, ok := target.(*ViewError)
	return ok && t != nil && t.Code == e.Code
}

// CapabilityUnavailableError reports required capabilities this view cannot
// serve at all: unavailable, or turned off by configuration. Retrying does not
// help — the caller must drop the requirement or pick another view.
type CapabilityUnavailableError struct {
	*ViewError
	// Capabilities lists the failing capabilities in the order the caller
	// declared them.
	Capabilities []CapabilityID `json:"capabilities"`
	// States records the state each failing capability was found in.
	States map[CapabilityID]CapabilityState `json:"states,omitempty"`
}

// Unwrap puts the embedded *ViewError in the unwrap chain. Without it the
// promoted Unwrap would return the ViewError's own cause and skip the
// ViewError itself, so errors.As(err, **ViewError) would never match.
func (e *CapabilityUnavailableError) Unwrap() error { return e.ViewError }

// RequiredCapabilityIncompleteError reports required capabilities that exist
// but are not finished: still building, or only partially populated. Unlike
// CapabilityUnavailableError, waiting and retrying can clear it.
type RequiredCapabilityIncompleteError struct {
	*ViewError
	// Capabilities lists the failing capabilities in the order the caller
	// declared them.
	Capabilities []CapabilityID `json:"capabilities"`
	// States records the state each failing capability was found in.
	States map[CapabilityID]CapabilityState `json:"states,omitempty"`
}

// Unwrap puts the embedded *ViewError in the unwrap chain, for the same
// reason CapabilityUnavailableError does.
func (e *RequiredCapabilityIncompleteError) Unwrap() error { return e.ViewError }

// newCapabilityUnavailable builds the typed capability_unavailable error.
func newCapabilityUnavailable(caps []CapabilityID, states map[CapabilityID]CapabilityState) *CapabilityUnavailableError {
	return &CapabilityUnavailableError{
		ViewError:    NewViewError(CodeCapabilityUnavailable, describeCapabilities(caps, states)),
		Capabilities: caps,
		States:       states,
	}
}

// newRequiredCapabilityIncomplete builds the typed
// required_capability_incomplete error.
func newRequiredCapabilityIncomplete(caps []CapabilityID, states map[CapabilityID]CapabilityState) *RequiredCapabilityIncompleteError {
	return &RequiredCapabilityIncompleteError{
		ViewError:    NewViewError(CodeRequiredCapabilityIncomplete, describeCapabilities(caps, states)),
		Capabilities: caps,
		States:       states,
	}
}

// describeCapabilities renders "id (state), id (state)" walking caps in order
// so the message is deterministic — the states map is never ranged over.
func describeCapabilities(caps []CapabilityID, states map[CapabilityID]CapabilityState) string {
	var b strings.Builder
	for i, id := range caps {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(string(id))
		if st, ok := states[id]; ok {
			b.WriteString(" (")
			b.WriteString(string(st))
			b.WriteString(")")
		}
	}
	return b.String()
}

// CodeOf returns the stable error code carried by err, or "" when err carries
// none. It walks the wrap chain, so a code survives being wrapped by a caller.
func CodeOf(err error) string {
	var c Coded
	if errors.As(err, &c) {
		return c.ErrorCode()
	}
	return ""
}

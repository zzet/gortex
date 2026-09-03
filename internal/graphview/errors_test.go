package graphview

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// sentinels pairs every code with the sentinel that must match it.
func sentinels() map[string]*ViewError {
	return map[string]*ViewError{
		CodeInvalidViewSelector:          ErrInvalidViewSelector,
		CodeRefNotCommit:                 ErrRefNotCommit,
		CodeSelectorConflict:             ErrSelectorConflict,
		CodeSelectorOutOfScope:           ErrSelectorOutOfScope,
		CodeRefNotAvailableLocally:       ErrRefNotAvailableLocally,
		CodeViewBuilding:                 ErrViewBuilding,
		CodeViewReadOnly:                 ErrViewReadOnly,
		CodeCapabilityUnavailable:        ErrCapabilityUnavailable,
		CodeRequiredCapabilityIncomplete: ErrRequiredCapabilityIncomplete,
		CodeCheckoutInaccessible:         ErrCheckoutInaccessible,
		CodeNoPrimary:                    ErrNoPrimary,
		CodePrimaryNotReady:              ErrPrimaryNotReady,
		CodeSourceObjectMissing:          ErrSourceObjectMissing,
	}
}

func TestErrorCodesAreStableStrings(t *testing.T) {
	want := []string{
		"invalid_view_selector",
		"ref_not_commit",
		"selector_conflict",
		"selector_out_of_scope",
		"ref_not_available_locally",
		"view_building",
		"view_read_only",
		"capability_unavailable",
		"required_capability_incomplete",
		"checkout_inaccessible",
		"no_primary",
		"primary_not_ready",
		"source_object_missing",
	}
	got := ErrorCodes()
	if len(got) != len(want) {
		t.Fatalf("ErrorCodes() returned %d codes, want %d", len(got), len(want))
	}
	for i, code := range want {
		if got[i] != code {
			t.Errorf("ErrorCodes()[%d] = %q, want %q", i, got[i], code)
		}
	}
	seen := map[string]bool{}
	for _, code := range got {
		if seen[code] {
			t.Errorf("code %q listed twice", code)
		}
		seen[code] = true
	}
}

func TestEveryCodeHasAMatchingSentinel(t *testing.T) {
	table := sentinels()
	if len(table) != len(ErrorCodes()) {
		t.Fatalf("sentinel table covers %d codes, ErrorCodes() lists %d", len(table), len(ErrorCodes()))
	}
	for _, code := range ErrorCodes() {
		sentinel, ok := table[code]
		if !ok {
			t.Fatalf("code %q has no sentinel", code)
		}
		if sentinel.Code != code {
			t.Errorf("sentinel for %q carries code %q", code, sentinel.Code)
		}
		if !errors.Is(NewViewError(code, "built here"), sentinel) {
			t.Errorf("a fresh error with code %q does not match its sentinel", code)
		}
	}
}

func TestSentinelsDoNotMatchOtherCodes(t *testing.T) {
	err := NewViewError(CodeViewBuilding, "still building")
	if errors.Is(err, ErrViewReadOnly) {
		t.Error("view_building matched the view_read_only sentinel")
	}
	if !errors.Is(err, ErrViewBuilding) {
		t.Error("view_building did not match its own sentinel")
	}
	if errors.Is(err, errors.New("view_building")) {
		t.Error("matched a plain error with the same text")
	}
}

func TestViewErrorMessageRendering(t *testing.T) {
	tests := []struct {
		name string
		err  *ViewError
		want string
	}{
		{"code only", &ViewError{Code: CodeNoPrimary}, "no_primary"},
		{"code and message", NewViewError(CodeNoPrimary, "nothing registered"), "no_primary: nothing registered"},
		{"wrapped cause", WrapViewError(CodeCheckoutInaccessible, "reading checkout", errors.New("permission denied")),
			"checkout_inaccessible: reading checkout: permission denied"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
	var nilErr *ViewError
	if got := nilErr.Error(); got != "<nil view error>" {
		t.Errorf("nil Error() = %q", got)
	}
	if got := nilErr.ErrorCode(); got != "" {
		t.Errorf("nil ErrorCode() = %q, want empty", got)
	}
	if nilErr.Is(ErrNoPrimary) {
		t.Error("nil view error matched a sentinel")
	}
}

func TestWrapViewErrorUnwrapsToCause(t *testing.T) {
	cause := errors.New("no such file")
	err := WrapViewError(CodeSourceObjectMissing, "blob 0f0f", cause)
	if !errors.Is(err, cause) {
		t.Error("wrapped error does not unwrap to its cause")
	}
	if !errors.Is(err, ErrSourceObjectMissing) {
		t.Error("wrapped error lost its code")
	}
	if got := errors.Unwrap(err); got != cause {
		t.Errorf("Unwrap() = %v, want %v", got, cause)
	}
	if got := errors.Unwrap(NewViewError(CodeNoPrimary, "x")); got != nil {
		t.Errorf("Unwrap() of an unwrapped error = %v, want nil", got)
	}
}

func TestViewErrorAsType(t *testing.T) {
	err := fmt.Errorf("resolving view: %w", NewViewError(CodeRefNotCommit, "points at a tree"))
	ve, ok := errors.AsType[*ViewError](err)
	if !ok {
		t.Fatal("errors.AsType did not find the view error")
	}
	if ve.Code != CodeRefNotCommit {
		t.Errorf("Code = %q, want %q", ve.Code, CodeRefNotCommit)
	}
	if ve.Message != "points at a tree" {
		t.Errorf("Message = %q", ve.Message)
	}
}

// TestCapabilityErrorsUnwrapToViewError pins the unwrap chain of the two
// capability error types. They embed *ViewError, so without their own Unwrap
// the promoted one would return the ViewError's cause and hide the ViewError
// itself from errors.As.
func TestCapabilityErrorsUnwrapToViewError(t *testing.T) {
	capErr := Completeness{}.Evaluate([]CapabilityID{CapSearchVector}, nil)
	incompleteErr := Completeness{CapSyntaxGraph: StateBuilding}.Evaluate([]CapabilityID{CapSyntaxGraph}, nil)

	t.Run("capability unavailable", func(t *testing.T) {
		typed, ok := errors.AsType[*CapabilityUnavailableError](capErr)
		if !ok {
			t.Fatalf("errors.AsType did not find %T", &CapabilityUnavailableError{})
		}
		ve, ok := errors.AsType[*ViewError](capErr)
		if !ok {
			t.Fatal("errors.AsType did not reach the embedded view error")
		}
		if ve != typed.ViewError {
			t.Errorf("AsType returned %p, want the embedded view error %p", ve, typed.ViewError)
		}
		if ve.Code != CodeCapabilityUnavailable {
			t.Errorf("Code = %q, want %q", ve.Code, CodeCapabilityUnavailable)
		}
		if !errors.Is(capErr, ErrCapabilityUnavailable) {
			t.Error("capability error stopped matching its sentinel")
		}
		if errors.Is(capErr, ErrRequiredCapabilityIncomplete) {
			t.Error("capability error matched the wrong sentinel")
		}
		if got := errors.Unwrap(capErr); got != typed.ViewError {
			t.Errorf("Unwrap() = %v, want the embedded view error", got)
		}
	})

	t.Run("required capability incomplete", func(t *testing.T) {
		typed, ok := errors.AsType[*RequiredCapabilityIncompleteError](incompleteErr)
		if !ok {
			t.Fatalf("errors.AsType did not find %T", &RequiredCapabilityIncompleteError{})
		}
		ve, ok := errors.AsType[*ViewError](incompleteErr)
		if !ok {
			t.Fatal("errors.AsType did not reach the embedded view error")
		}
		if ve != typed.ViewError {
			t.Errorf("AsType returned %p, want the embedded view error %p", ve, typed.ViewError)
		}
		if ve.Code != CodeRequiredCapabilityIncomplete {
			t.Errorf("Code = %q, want %q", ve.Code, CodeRequiredCapabilityIncomplete)
		}
		if !errors.Is(incompleteErr, ErrRequiredCapabilityIncomplete) {
			t.Error("incomplete error stopped matching its sentinel")
		}
		if errors.Is(incompleteErr, ErrCapabilityUnavailable) {
			t.Error("incomplete error matched the wrong sentinel")
		}
		if got := errors.Unwrap(incompleteErr); got != typed.ViewError {
			t.Errorf("Unwrap() = %v, want the embedded view error", got)
		}
	})

	t.Run("still reachable through an outer wrap", func(t *testing.T) {
		wrapped := fmt.Errorf("checking capabilities: %w", capErr)
		ve, ok := errors.AsType[*ViewError](wrapped)
		if !ok {
			t.Fatal("errors.AsType did not reach the view error through the wrap")
		}
		if ve.Code != CodeCapabilityUnavailable {
			t.Errorf("Code = %q, want %q", ve.Code, CodeCapabilityUnavailable)
		}
		if !errors.Is(wrapped, ErrCapabilityUnavailable) {
			t.Error("wrapped capability error lost its sentinel match")
		}
	})
}

// TestCapabilityErrorMessageRendering pins the rendered message of both
// capability errors, which the added Unwrap must not disturb.
func TestCapabilityErrorMessageRendering(t *testing.T) {
	capErr := Completeness{CapSearchVector: StateDisabledByConfig}.Evaluate([]CapabilityID{CapSearchVector}, nil)
	const wantCap = "capability_unavailable: search.vector (disabled_by_config)"
	if got := capErr.Error(); got != wantCap {
		t.Errorf("Error() = %q, want %q", got, wantCap)
	}

	incompleteErr := Completeness{CapSyntaxGraph: StateBuilding}.Evaluate([]CapabilityID{CapSyntaxGraph}, nil)
	const wantIncomplete = "required_capability_incomplete: graph.syntax (building)"
	if got := incompleteErr.Error(); got != wantIncomplete {
		t.Errorf("Error() = %q, want %q", got, wantIncomplete)
	}
}

func TestCodeOf(t *testing.T) {
	capErr := Completeness{}.Evaluate([]CapabilityID{CapSearchVector}, nil)
	incompleteErr := Completeness{CapSyntaxGraph: StateBuilding}.Evaluate([]CapabilityID{CapSyntaxGraph}, nil)

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"plain error", errors.New("boom"), ""},
		{"view error", NewViewError(CodeViewReadOnly, "pinned"), CodeViewReadOnly},
		{"wrapped view error", fmt.Errorf("outer: %w", NewViewError(CodeNoPrimary, "")), CodeNoPrimary},
		{"capability unavailable", capErr, CodeCapabilityUnavailable},
		{"capability incomplete", incompleteErr, CodeRequiredCapabilityIncomplete},
		{"wrapped capability error", fmt.Errorf("outer: %w", capErr), CodeCapabilityUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CodeOf(tc.err); got != tc.want {
				t.Errorf("CodeOf() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCodedInterfaceIsImplemented(t *testing.T) {
	var c Coded = NewViewError(CodeViewBuilding, "warming up")
	if c.ErrorCode() != CodeViewBuilding {
		t.Errorf("ErrorCode() = %q", c.ErrorCode())
	}
	capErr := Completeness{}.Evaluate([]CapabilityID{CapLSPHover}, nil)
	var asCoded Coded
	if !errors.As(capErr, &asCoded) {
		t.Fatal("capability error does not satisfy Coded")
	}
	if asCoded.ErrorCode() != CodeCapabilityUnavailable {
		t.Errorf("ErrorCode() = %q", asCoded.ErrorCode())
	}
}

func TestViewErrorJSONShape(t *testing.T) {
	blob, err := json.Marshal(NewViewError(CodeViewBuilding, "warming up"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"code":"view_building","message":"warming up"}`
	if string(blob) != want {
		t.Errorf("json = %s, want %s", blob, want)
	}

	capErr := Completeness{CapSearchVector: StateDisabledByConfig}.Evaluate([]CapabilityID{CapSearchVector}, nil)
	blob, err = json.Marshal(capErr)
	if err != nil {
		t.Fatalf("marshal capability error: %v", err)
	}
	const wantCap = `{"code":"capability_unavailable","message":"search.vector (disabled_by_config)",` +
		`"capabilities":["search.vector"],"states":{"search.vector":"disabled_by_config"}}`
	if string(blob) != wantCap {
		t.Errorf("json = %s, want %s", blob, wantCap)
	}
}

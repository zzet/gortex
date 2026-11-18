package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

// A ref view reads a graph's committed payload, so it is a reader of that
// repository and has to be admitted like one: EnsureRefView acquires a
// repository read on the graph before it touches the catalog
// (internal/indexer/ref_view_service.go:44-48), and a read is only admitted
// against an owner this process registered through the lifecycle's privileged
// boundary. Without that pin the payload a served view is composing over could
// be purged out from under it by a concurrent cleanup.
//
// The catalog alone cannot stand in for the registration. Rows saying a
// dedicated graph is ready are durable state a previous process wrote; the
// admission is this process's own promise not to purge what it is serving.
// That is precisely why the ref-view fixture has to make the promise
// explicitly, and these tests are what keeps the gate from being mistaken for
// the fixture's problem and deleted.

// TestRefViewRefusesAGraphThisProcessNeverAdmitted pins the gate itself: a
// catalog-ready graph whose owner was never registered serves no committed
// bytes, and the refusal is legible as the admission failure it is.
func TestRefViewRefusesAGraphThisProcessNeverAdmitted(t *testing.T) {
	stack := newUnadmittedRefStack(t)

	// The lifecycle boundary refuses first, with the typed error the admission
	// domain raises — not a catalog miss and not a git failure.
	if _, err := stack.lifecycle.EnsureRefView(context.Background(), indexer.RefViewSelection{
		GraphID:       stack.graphID,
		SelectorKind:  gitstate.ViewSelectorGitRef,
		SelectorValue: "refs/heads/feature",
	}); !errors.Is(err, graphview.ErrRepositoryOwnerUnknown) {
		t.Fatalf("EnsureRefView on an unadmitted graph = %v, want %v",
			err, graphview.ErrRepositoryOwnerUnknown)
	}

	// And the whole request path refuses with it, rather than falling through
	// to the working copy the canonical checkout still holds.
	res, err := stack.readFile(t, refSelector("git_ref", "refs/heads/feature"), "repo/edit.go")
	if err != nil {
		t.Fatalf("read through the unadmitted ref view: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("an unadmitted ref view answered instead of refusing: %+v", res)
	}
	text := viewResultText(t, res)
	if !strings.Contains(text, graphview.ErrRepositoryOwnerUnknown.Error()) {
		t.Errorf("the refusal does not name the admission failure:\n%s", text)
	}
	// The bytes of neither tree may ride on a refusal: not the branch's
	// committed content, and not the main corpus's working copy.
	if strings.Contains(text, "func New()") || strings.Contains(text, "func Old()") {
		t.Errorf("a refused ref view leaked file content:\n%s", text)
	}

	// The registration is the whole of it: with the owner admitted the very
	// same request serves the committed tree. Without this control the test
	// above would also pass on a fixture that is broken for some other reason.
	if err := stack.lifecycle.RegisterRepositoryOwner(context.Background(), stack.graphID); err != nil {
		t.Fatalf("register the repository owner: %v", err)
	}
	served, err := stack.readFile(t, refSelector("git_ref", "refs/heads/feature"), "repo/edit.go")
	if err != nil {
		t.Fatalf("read through the admitted ref view: %v", err)
	}
	if served.IsError {
		t.Fatalf("the admitted ref view was still refused: %s", viewResultText(t, served))
	}
	if content := refResultString(t, refResultObject(t, served), "content"); !strings.Contains(content, "func New()") {
		t.Fatalf("the admitted ref view did not serve the branch's committed content:\n%s", content)
	}
}

// TestRefViewAdmissionSurvivesAnIdempotentReRegistration pins that the fixture's
// registration is the ordinary lifecycle one and not a one-shot: re-registering
// the exact open owner is idempotent (graphview.(*LeaseManager).RegisterRepositoryOwner,
// repository_lease.go:86-92), so a second seeding pass over the same catalog
// rows neither fails nor withdraws the admission a live reader is holding.
func TestRefViewAdmissionSurvivesAnIdempotentReRegistration(t *testing.T) {
	stack := newRefStack(t)

	if err := stack.lifecycle.RegisterRepositoryOwner(context.Background(), stack.graphID); err != nil {
		t.Fatalf("re-register the exact open owner: %v", err)
	}

	res, err := stack.readFile(t, refSelector("git_ref", "refs/heads/feature"), "repo/edit.go")
	if err != nil {
		t.Fatalf("read through the ref view: %v", err)
	}
	if res.IsError {
		t.Fatalf("a re-registration refused a served view: %s", viewResultText(t, res))
	}
	if content := refResultString(t, refResultObject(t, res), "content"); !strings.Contains(content, "func New()") {
		t.Fatalf("the ref view stopped serving the committed content:\n%s", content)
	}
}

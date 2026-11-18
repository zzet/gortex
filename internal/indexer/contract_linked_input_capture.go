package indexer

import (
	"context"
	"fmt"
	"slices"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

// ContractLinkedInputCapture reacquires an independently owned source from a
// stable checkout/cohort identity. The caller owns a returned source even on
// failure; no previously consumed source may satisfy this callback.
type ContractLinkedInputCapture func(context.Context, ContractActor, string) (*graphview.SelectedContractInputs, *ContractCaptureSource, error)

// NewContractLinkedInputCapture rematerializes the current checkout route.
// Companion captures preserve the selected chain's positive-layer absences;
// neither a primary capture nor an old selected view substitutes for it.
func NewContractLinkedInputCapture(materializer *graphview.Materializer) ContractLinkedInputCapture {
	return func(ctx context.Context, actor ContractActor, repo string) (*graphview.SelectedContractInputs, *ContractCaptureSource, error) {
		repos := actor.Repos()
		if ctx == nil || materializer == nil || actor.Checkout == "" || len(repos) == 0 || !slices.Contains(repos, repo) {
			return nil, nil, graph.ErrContractInputVector
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		view, err := materializer.MaterializeCheckout(ctx, actor.Checkout)
		if err != nil {
			return nil, nil, err
		}
		defer view.Close()
		captures := make([]*graphview.SelectedContractInputs, 0, len(repos))
		for _, name := range repos {
			var input *graphview.SelectedContractInputs
			if name == view.ID.RepoPrefix {
				input, err = materializer.CaptureContractInputs(ctx, view, name, actor.Checkout)
			} else {
				input, err = materializer.CaptureContractCompanionInputs(ctx, view, name, actor.Checkout)
			}
			if err != nil {
				return nil, nil, err
			}
			captures = append(captures, input)
		}
		inputs, err := graphview.ComposeSelectedContractInputs(repo, actor.Checkout, captures...)
		if err != nil {
			return nil, nil, err
		}
		if err := inputs.Validate(ctx); err != nil {
			return nil, nil, err
		}
		source, err := PrepareContractCaptureSource(view)
		if err != nil {
			return nil, nil, err
		}
		owned := true
		defer func() {
			if owned {
				source.Release()
			}
		}()
		if source == nil {
			return nil, nil, fmt.Errorf("contract linked capture: missing source handoff")
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if err := inputs.Validate(ctx); err != nil {
			return nil, nil, err
		}
		owned = false
		return inputs, source, nil
	}
}

func contractLinkedInputsMatch(actor ContractActor, repo string, inputs *graphview.SelectedContractInputs) bool {
	if inputs == nil || inputs.State.RepoPrefix != repo || inputs.State.CheckoutID != actor.Checkout {
		return false
	}
	repos := actor.Repos()
	found := map[string]bool{}
	for _, witness := range inputs.Witnesses {
		if !slices.Contains(repos, witness.State.RepoPrefix) {
			return false
		}
		found[witness.State.RepoPrefix] = true
	}
	return len(found) == len(repos)
}

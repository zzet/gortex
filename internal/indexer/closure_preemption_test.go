package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/zzet/gortex/internal/indexer/source"
)

// cancelOnOpenSource cancels its context once n changed files were opened:
// the moment an interactive build starts waiting for the lane a background
// build holds.
type cancelOnOpenSource struct {
	semanticPlanningSource
	cancel context.CancelFunc
	after  int
	opens  int
}

func (s *cancelOnOpenSource) Open(path string) (io.ReadCloser, source.FileMeta, error) {
	s.opens++
	if s.opens == s.after {
		s.cancel()
	}
	return s.semanticPlanningSource.Open(path)
}

// TestClosureOfALargeDirtySetStopsWhenPreempted pins that the closure walk
// notices a canceled build context between the changed files it parses: a
// background build over hundreds of dirty files spends tens of seconds in that
// loop, and an interactive edit waiting for the build lane preempts it by
// canceling the context — which the walk ignored until the loop finished (live:
// the measurement checkout's edits waited 30–70 s for the lane).
func TestClosureOfALargeDirtySetStopsWhenPreempted(t *testing.T) {
	builder, _, req := benchmarkSemanticReverseFanoutFixture(4)
	base := req.Target.(semanticPlanningSource)
	present := map[string]struct{}{}
	for i := 0; i < 64; i++ {
		rel := fmt.Sprintf("dirty_%02d.go", i)
		body := fmt.Sprintf("package fixture\n\nfunc Dirty%02d() int { return %d }\n", i, i)
		base.files[rel] = source.FileMeta{Path: rel, Size: int64(len(body))}
		base.bodies[rel] = body
		present[rel] = struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := &cancelOnOpenSource{semanticPlanningSource: base, cancel: cancel, after: 3}
	req.Target = target

	var report BuildReport
	_, err := builder.affectedClosureContext(ctx, req, present, nil, &report)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("closure over a preempted build returned %v, want context.Canceled", err)
	}
	if target.opens > 4 {
		t.Fatalf("the walk parsed %d of %d changed files after its build was preempted at the 3rd", target.opens, len(present))
	}
}

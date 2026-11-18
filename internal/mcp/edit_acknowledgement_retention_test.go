package mcp

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEditAcknowledgementPhysicalFilesShareOneRPCButRetriesDoNot(t *testing.T) {
	s := newMutationServer()
	ctx, first, owner := s.withEditAcknowledgement(canonicalTimingContext())
	require.True(t, owner)
	root := t.TempDir()
	a := commitTimingFixture(t, s, ctx, filepath.Join(root, "a.go"), "first")
	b := commitTimingFixture(t, s, ctx, filepath.Join(root, "b.go"), "second")
	require.Same(t, first, retainedMutationAcknowledgement(a))
	require.Same(t, first, retainedMutationAcknowledgement(b), "two physical files must not multiply the RPC wall")
	retryCtx, retry, retryOwner := s.withEditAcknowledgement(canonicalTimingContext())
	require.True(t, retryOwner)
	c := commitTimingFixture(t, s, retryCtx, filepath.Join(root, "a.go"), "third")
	require.Same(t, retry, retainedMutationAcknowledgement(c))
	require.NotEqual(t, first.id, retry.id)
	// An unobserved/native/raw commit has no invented MCP acknowledgement.
	raw := commitTimingFixture(t, s, canonicalTimingContext(), filepath.Join(root, "raw.go"), "raw")
	require.Nil(t, retainedMutationAcknowledgement(raw))
}

func TestEditAcknowledgementHistoryEndsAtLedgerExpiryAndCap(t *testing.T) {
	for _, expire := range []bool{true, false} {
		t.Run(fmt.Sprintf("expire=%t", expire), func(t *testing.T) {
			s := newMutationServer()
			ctx, ack, _ := s.withEditAcknowledgement(canonicalTimingContext())
			r := commitTimingFixture(t, s, ctx, filepath.Join(t.TempDir(), "held.go"), "held")
			require.Same(t, ack, retainedMutationAcknowledgement(r))
			// Keep a physical receipt alive independently, as failed checkout
			// source evidence can. It must not retain acknowledgement history.
			if expire {
				r.mu.Lock()
				r.startedAt = time.Now().Add(-2 * mutationCommitRetention)
				r.mu.Unlock()
			}
			count := 1
			if !expire {
				count = maxMutationCommits
			}
			for i := range count {
				s.mutationCommits.put(&mutationCommitRecord{id: fmt.Sprintf("ack-cap-%d", i), startedAt: time.Now()})
			}
			_, found := s.mutationCommits.byReceipt(r.id)
			require.False(t, found)
			require.Nil(t, retainedMutationAcknowledgement(r))
			ack.finishReplyAt(ack.entered.Add(time.Millisecond), "result")
			ack.finishActualAt(ack.entered.Add(2 * time.Millisecond))
			require.Nil(t, retainedMutationAcknowledgement(r), "late completion cannot reinstall an evicted pointer")
			// The still-running call can finish its own private observation;
			// that is not a resurrected history owner or a new registry.
			require.Equal(t, "measured", ack.snapshot().ReplyState)
		})
	}
}

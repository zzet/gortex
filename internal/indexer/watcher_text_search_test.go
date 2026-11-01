package indexer

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// All committed-mutation front doors must expose the written bytes while the
// graph patch is deliberately held back; admission does not wait on reindex.
func TestWatcherAdmittedEditVisibleToTextSearch(t *testing.T) {
	for _, admission := range []string{"debounced", "tool", "set"} {
		t.Run(admission, func(t *testing.T) {
			dir, idx, w := inertTestWatcher(t, "main.go", "package main\n\nfunc value() int { return 0 }\n")
			now := time.Unix(1_700_000_000, 0)
			idx.trigramBudgetOverride = newTestTrigramBudget(time.Hour, 3, &now)
			require.NotEmpty(t, idx.GrepText("func value", 10))
			require.NotNil(t, idx.trigramSearcher, "the searcher is warm")

			path := filepath.Join(dir, "main.go")
			w.patchMu.Lock()
			var unlocked sync.Once
			unlock := func() { unlocked.Do(w.patchMu.Unlock) }
			defer unlock()
			writeTestFile(t, path, "package main\n\nfunc value() int { return admittedValue }\n\nconst admittedValue = 1\n")
			var tickets []*MutationTicket
			switch admission {
			case "debounced", "tool":
				admit := w.EnqueueFileMutation
				if admission == "tool" {
					admit = w.EnqueueToolMutation
				}
				ticket, err := admit(context.Background(), path)
				require.NoError(t, err)
				require.NotNil(t, ticket)
				tickets = append(tickets, ticket)
			case "set":
				side := filepath.Join(dir, "side.go")
				writeTestFile(t, side, "package main\nconst sideValue = 1\n")
				admitted, err := w.EnqueueFileMutations(context.Background(), []string{path, side})
				require.NoError(t, err)
				require.Len(t, admitted, 2)
				for _, ticket := range admitted {
					tickets = append(tickets, ticket)
				}
				require.Len(t, idx.GrepText("sideValue", 10), 1, "a newly written untracked member must be searchable before the set drains")
			}
			matches := idx.GrepText("admittedValue", 10)
			unlock()
			for _, ticket := range tickets {
				select {
				case <-ticket.Done:
				case <-time.After(5 * time.Second):
					t.Fatal("the admitted edit was never reindexed")
				}
			}
			require.Len(t, matches, 2)
			require.Equal(t, 3, matches[0].Line)
			require.Equal(t, 5, matches[1].Line)
		})
	}
}

package store_sqlite

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGenerationMaskContextReadsCancelWhileWaitingForConnection(t *testing.T) {
	tests := []struct {
		name string
		read func(context.Context, *Store, []NodeIdentityMask) error
	}{
		{
			name: "file masks",
			read: func(ctx context.Context, store *Store, _ []NodeIdentityMask) error {
				_, err := store.FileMasksContext(ctx)
				return err
			},
		},
		{
			name: "node identity masks",
			read: func(ctx context.Context, store *Store, _ []NodeIdentityMask) error {
				_, err := store.NodeIdentityMasksContext(ctx)
				return err
			},
		},
		{
			name: "edge source masks",
			read: func(ctx context.Context, store *Store, _ []NodeIdentityMask) error {
				_, err := store.EdgeSourceMasksContext(ctx)
				return err
			},
		},
		{
			name: "node identity summaries",
			read: func(ctx context.Context, store *Store, masks []NodeIdentityMask) error {
				_, err := store.NodeIdentityMaskSummariesContext(ctx, masks)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openMaskStore(t).AtGeneration(1)
			if err := store.SetFileMasks([]FileMask{{
				RepoPrefix: maskTestRepo,
				FilePath:   "repo/a.go",
				Mode:       OwnershipReplace,
			}}); err != nil {
				t.Fatalf("SetFileMasks: %v", err)
			}
			if err := store.SetNodeTombstones([]string{"repo/a.go::Gone"}); err != nil {
				t.Fatalf("SetNodeTombstones: %v", err)
			}
			if err := store.SetEdgeSourceMasks([]EdgeSourceMask{{
				SourceID: "repo/a.go::Caller",
				Mode:     OwnershipReplace,
			}}); err != nil {
				t.Fatalf("SetEdgeSourceMasks: %v", err)
			}
			masks, err := store.NodeIdentityMasksContext(context.Background())
			if err != nil {
				t.Fatalf("NodeIdentityMasksContext setup: %v", err)
			}
			if len(masks) == 0 {
				t.Fatal("NodeIdentityMasksContext setup returned no masks")
			}

			store.db.SetMaxOpenConns(1)
			store.db.SetMaxIdleConns(1)
			held, err := store.db.Conn(context.Background())
			if err != nil {
				t.Fatalf("hold sole read connection: %v", err)
			}
			t.Cleanup(func() { _ = held.Close() })

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			done := make(chan error, 1)
			before := store.db.Stats().WaitCount
			go func() {
				done <- test.read(ctx, store, masks)
			}()

			deadline := time.Now().Add(2 * time.Second)
			for store.db.Stats().WaitCount == before {
				if time.Now().After(deadline) {
					cancel()
					t.Fatal("context read did not wait for the held connection")
				}
				time.Sleep(time.Millisecond)
			}
			cancel()

			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("context read error = %v, want context.Canceled", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("context read did not return after cancellation")
			}

			if err := held.Close(); err != nil {
				t.Fatalf("release held connection: %v", err)
			}
			if _, err := store.FileMasksContext(context.Background()); err != nil {
				t.Fatalf("read pool unusable after cancellation: %v", err)
			}
		})
	}
}

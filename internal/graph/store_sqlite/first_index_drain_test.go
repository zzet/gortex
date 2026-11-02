package store_sqlite

import (
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestFirstIndexDrainSnapshotAvoidsWriterAndCallerMutation(t *testing.T) {
	s, _ := openTempStore(t)
	s.RecordFirstIndexDrain("repo", graph.FirstIndexDrain{Window: "standalone", Nodes: 3})
	s.writeMu.Lock()
	result := make(chan map[string]graph.FirstIndexDrain, 1)
	go func() { result <- s.LastFirstIndexDrains() }()
	var snapshot map[string]graph.FirstIndexDrain
	select {
	case snapshot = <-result:
		s.writeMu.Unlock()
	case <-time.After(5 * time.Second):
		s.writeMu.Unlock()
		t.Fatal("status snapshot queued behind writer")
	}
	delete(snapshot, "repo")
	actual := s.LastFirstIndexDrains()
	if actual["repo"].Nodes != 3 {
		t.Fatalf("caller changed retained observation: %v", actual)
	}
	if err := s.RekeyRepoPrefix("repo", "renamed"); err != nil {
		t.Fatal(err)
	}
	if got := s.LastFirstIndexDrains(); len(got) != 1 || got["renamed"].Nodes != 3 {
		t.Fatalf("rekey lost or duplicated observation: %v", got)
	}
	if err := s.PurgeRepo("renamed"); err != nil {
		t.Fatal(err)
	}
	if got := s.LastFirstIndexDrains(); len(got) != 0 {
		t.Fatalf("purged repository retained observation: %v", got)
	}
}

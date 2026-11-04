package store_sqlite

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type bulkAdmissionLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *bulkAdmissionLogBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(data)
}
func (b *bulkAdmissionLogBuffer) reset() { b.mu.Lock(); defer b.mu.Unlock(); b.Reset() }
func (b *bulkAdmissionLogBuffer) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.String()
}

func TestBulkBeginReportsEveryAdmissionOutcome(t *testing.T) {
	s, _ := openTempStore(t)
	var output bulkAdmissionLogBuffer
	prior := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(prior) })
	expect := func(window, outcome, reason string, begin func()) {
		t.Helper()
		output.reset()
		begin()
		lines := strings.Split(strings.TrimSpace(output.text()), "\n")
		var admissions []string
		for _, line := range lines {
			if strings.Contains(line, "bulk begin ") {
				admissions = append(admissions, line)
			}
		}
		want := "window=" + window + " outcome=" + outcome + " reason=" + reason
		if len(admissions) != 1 || !strings.Contains(admissions[0], want) {
			t.Fatalf("admission logs=%q, want exactly one %q", admissions, want)
		}
	}
	expect("standalone", "engaged", "cold_store", s.BeginBulkLoad)
	expect("standalone", "nested_noop", "active_window", s.BeginBulkLoad)
	expect("coordinated", "nested_noop", "active_window", func() { s.BeginCoordinatedBulkLoad() })
	if err := s.FlushBulk(); err != nil {
		t.Fatal(err)
	}
	s.AddBatch([]*graph.Node{{ID: "repo/a.go::A", Name: "A", Kind: graph.KindFunction}}, nil)
	expect("standalone", "refused_not_cold", "nodes", s.BeginBulkLoad)
	expect("coordinated", "refused_not_cold", "nodes", func() { s.BeginCoordinatedBulkLoad() })
	memory, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer memory.Close()
	expect("standalone", "memory_store", "memory_store", memory.BeginBulkLoad)
	expect("coordinated", "memory_store", "memory_store", func() { memory.BeginCoordinatedBulkLoad() })
	for _, predicate := range []string{"edges", "file_mtimes", "repo_index_state"} {
		warm, _ := openTempStore(t)
		switch predicate {
		case "edges":
			nodes, edges := bulkFixture(2, 1)
			warm.AddBatch(nodes, edges)
			if _, err := warm.writerDB.Exec("DELETE FROM nodes"); err != nil {
				t.Fatal(err)
			}
		case "file_mtimes":
			if err := warm.SetFileMtime("repo", "a.go", 1); err != nil {
				t.Fatal(err)
			}
		case "repo_index_state":
			if err := warm.SetRepoIndexState(graph.RepoIndexState{RepoPrefix: "repo"}); err != nil {
				t.Fatal(err)
			}
		}
		expect("coordinated", "refused_not_cold", predicate, func() { warm.BeginCoordinatedBulkLoad() })
	}
	generation, _ := openTempStore(t)
	if active, err := generation.BeginGenerationBulkLoad(42); err != nil || !active {
		t.Fatalf("generation active=%v err=%v", active, err)
	}
	expect("standalone", "refused_generation_window", "generation_window", generation.BeginBulkLoad)
	expect("coordinated", "refused_generation_window", "generation_window", func() { generation.BeginCoordinatedBulkLoad() })
}

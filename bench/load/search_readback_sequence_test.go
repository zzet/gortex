package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// Exercise the real multiplexed client and doEdit continuation, without a
// daemon, filesystem fixture or shared socket namespace.
func searchSequenceClient(t *testing.T, handle func(stubFrame) any) *socketClient {
	t.Helper()
	local, peer := net.Pipe()
	c := &socketClient{conn: local, reader: bufio.NewReader(local), pending: make(map[int64]chan wireReply), done: make(chan struct{}), writeGate: make(chan struct{}, 1)}
	go c.readLoop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var workers sync.WaitGroup
		defer workers.Wait()
		var writes sync.Mutex
		decoder := json.NewDecoder(peer)
		for {
			var frame stubFrame
			if decoder.Decode(&frame) != nil {
				return
			}
			workers.Go(func() {
				result := handle(frame)
				writes.Lock()
				defer writes.Unlock()
				_ = json.NewEncoder(peer).Encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": result})
			})
		}
	}()
	t.Cleanup(func() { _ = c.Close(); _ = peer.Close(); <-done })
	return c
}

func TestSearchReadbackSameEditorWaitsForAllFourWitnesses(t *testing.T) {
	for _, held := range []int{0, 3} {
		t.Run(fmt.Sprint(held), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			reached, release := make(chan struct{}), make(chan struct{})
			var released sync.Once
			unblock := func() { released.Do(func() { close(release) }) }
			defer unblock()
			firstQuery := ""
			edits := make(chan int, 3)
			var searchCount int
			var mu sync.Mutex
			client := func(editor int) *socketClient {
				return searchSequenceClient(t, func(frame stubFrame) any {
					args, _ := frame.Params["arguments"].(map[string]any)
					if frame.Params["name"] == "edit" {
						edits <- editor
						return stubContent(map[string]any{"mutation_receipt": "owned-receipt"})
					}
					if editor == 0 && frame.Params["name"] == "search" {
						query, _ := args["query"].(string)
						if strings.Contains(query, firstQuery) {
							mu.Lock()
							index := searchCount
							searchCount++
							mu.Unlock()
							if index == held {
								close(reached)
								<-release
							}
						}
						return stubContent(map[string]any{})
					}
					// After the four searches, unrelated diagnostic read/status
					// work must not retain the editor's acceptance turn.
					<-ctx.Done()
					return stubContent(map[string]any{})
				})
			}
			corpus := editProbeRenameCorpus(t, "go", 1000)
			corpus.EditTool = "edit.file"
			plan, err := makeEditProbeTrafficPlanSized(corpus, 42, 1000, 8, time.Second, true)
			if err != nil {
				t.Fatal(err)
			}
			plan.SearchReadbackAcceptance = true
			plan.Steps = plan.Steps[:3]
			plan.Steps[0].Editor, plan.Steps[1].Editor, plan.Steps[2].Editor = 0, 0, 1
			firstQuery = plan.Steps[0].Marker
			r := &editProbeRuntime{plan: plan, options: runOptions{Deadline: 2 * time.Second}, p: &privateRun{Repos: []privateRepo{{Prefix: "fixture", Corpus: corpus}}}, phase: &Phase{Metrics: make(map[string]Metric)}, origin: time.Now()}
			r.editors = []*socketClient{client(0), client(1)}
			for _, step := range plan.Steps {
				sink := newEditProbeHeldSink(t)
				sink.unblock()
				s := startHeldEditProbeSpool(t, sink)
				s.step, s.record = step, newEditProbeRecord(corpus, step)
				s.ackReady = make(chan struct{})
				r.states = append(r.states, s)
			}
			var tasks sync.WaitGroup
			start := func(i int) { tasks.Go(func() { r.doEdit(ctx, plan.Steps[i]) }) }
			start(0)
			select {
			case editor := <-edits:
				if editor != 0 {
					t.Fatal("first edit used another session")
				}
			case <-ctx.Done():
				t.Fatal("first edit did not dispatch")
			}
			awaitEditProbeSpool(t, reached)
			start(1)
			start(2)
			select {
			case editor := <-edits:
				if editor != 1 {
					t.Error("same-session second edit overtook the first four readbacks")
				}
			case <-ctx.Done():
				t.Error("another editor was blocked by the first editor's readback")
			}
			select {
			case <-edits:
				t.Error("same-session edit dispatched while its predecessor readback was held")
			case <-time.After(30 * time.Millisecond):
			}
			unblock()
			select {
			case editor := <-edits:
				if editor != 0 {
					t.Error("queued edit changed its fixed editor session")
				}
			case <-ctx.Done():
				t.Error("completed four-search turn failed to release before diagnostics")
			}
			cancel()
			tasks.Wait()
		})
	}
}

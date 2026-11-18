package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// A blocked first read must hold only its own matched transaction, never let
// the partner edit get ahead of that read or hold another editor stream.
func TestPairedOwnACKTransactionReadBeforePartnerEdit(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reading, release, partner := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var readOnce, releaseOnce, partnerOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	c := pairedEditProbeFixture(t, ctx, 1, func(label string, frame stubFrame) (any, *rpcError) {
		if frame.Params["name"] == "edit" {
			if label == "candidate" {
				partnerOnce.Do(func() { close(partner) })
			}
			return stubContent(map[string]any{"mutation_receipt": label + "-receipt"}), nil
		}
		if label == "baseline" && frame.Params["name"] == "read" {
			readOnce.Do(func() { close(reading) })
			<-release
		}
		return stubContent(map[string]any{}), nil // incorrect reads must remain explicit
	})
	c.options.PairedOwnACKTransactions = true
	done := make(chan struct{})
	t.Cleanup(func() { unblock(); cancel(); <-done; c.editContinuations.Wait() })
	go func() {
		defer close(done)
		step := c.states[0].phase.editProbeRuntime.plan.Steps[0]
		c.callAcross(ctx, step.Request, 0, 0, 0, 0, "workload", time.Second, -1)
	}()
	awaitEditProbeSpool(t, reading)
	select {
	case <-partner:
		t.Fatal("partner edit dispatched before first transaction's file/source reads finished")
	default:
	}
	unblock()
	awaitEditProbeSpool(t, partner)
	awaitEditProbeSpool(t, done)
	var transaction *pairedRecord
	for i := range c.records {
		if c.records[i].Tuple.Role == "transaction" {
			transaction = &c.records[i]
		}
	}
	if transaction == nil || len(transaction.Samples) != 2 {
		t.Fatal("failed readbacks erased the transaction denominator")
	}
	for _, sample := range transaction.Samples {
		if sample.Outcome == "success" {
			t.Fatal("incorrect/nonfresh readbacks counted as a successful transaction")
		}
	}
	for _, categories := range matchedLatencyRatios(c.labels, c.records) {
		row := categories["transaction."+pairedOwnACKTransactionMethod]
		if row.Matched != 0 || row.Excluded != 1 || row.Median.Value != nil {
			t.Fatalf("failure became a successful matched ratio: %+v", row)
		}
	}
}

func TestPairedOwnACKTransactionStreamsRemainConcurrent(t *testing.T) {
	for _, streams := range []int{4, 8} {
		t.Run(fmt.Sprint(streams), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			entered, release := make(chan struct{}, streams), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			c := pairedEditProbeFixture(t, ctx, streams, func(_ string, frame stubFrame) (any, *rpcError) {
				if frame.Params["name"] == "edit" {
					return stubContent(map[string]any{"mutation_receipt": "receipt"}), nil
				}
				if frame.Params["name"] == "read" {
					args, _ := frame.Params["arguments"].(map[string]any)
					if args["operation"] == "file" {
						entered <- struct{}{}
						<-release
					}
				}
				return stubContent(map[string]any{}), nil
			})
			c.options.PairedOwnACKTransactions = true
			for _, state := range c.states {
				r := state.phase.editProbeRuntime
				for i := 1; i < streams; i++ {
					client, err := openMCP(ctx, state.p.Socket, "/owned/editor")
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = client.Close() })
					r.editors = append(r.editors, client)
				}
				for i := range r.plan.Steps {
					r.plan.Steps[i].Editor = i
					r.states[i].step.Editor = i
				}
			}
			var calls sync.WaitGroup
			t.Cleanup(func() { unblock(); cancel(); calls.Wait(); c.editContinuations.Wait() })
			for i := 0; i < streams; i++ {
				calls.Go(func() {
					step := c.states[0].phase.editProbeRuntime.plan.Steps[i]
					c.callAcross(ctx, step.Request, 0, i, i, 0, "workload", time.Second, -1)
				})
			}
			for i := 0; i < streams; i++ {
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("transaction wait serialized independent editor streams")
				}
			}
			unblock()
			calls.Wait()
		})
	}
}

func TestPairedOwnACKTransactionOptInValidation(t *testing.T) {
	base := []string{"--build", "baseline=/private/base", "--build", "candidate=/private/candidate", "--manifest", "manifest", "--scratch", "scratch", "--scenario", editProbeScenario, "--agents", "0", "--worktrees", "0"}
	for _, editors := range []int{4, 8} {
		args := append(append([]string{}, base...), "--edit-agents", fmt.Sprint(editors), "--paired-own-ack-transactions")
		if _, _, err := parsePaired(args); err != nil {
			t.Fatalf("approved %d-stream mode refused: %v", editors, err)
		}
		if _, _, err := parsePaired(append(append([]string{}, base...), "--edit-agents", fmt.Sprint(editors))); err == nil {
			t.Fatal("legacy paired method silently broadened")
		}
	}
	args := append(append([]string{}, base...), "--edit-agents", "1", "--paired-own-ack-transactions")
	if _, _, err := parsePaired(args); err == nil || !strings.Contains(err.Error(), "4 or 8") {
		t.Fatalf("unapproved transaction shape accepted: %v", err)
	}
}

func TestPairedOwnACKTransactionCancellationRetainsBothBuilds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	reading, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	c := pairedEditProbeFixture(t, ctx, 1, func(_ string, frame stubFrame) (any, *rpcError) {
		if frame.Params["name"] == "edit" {
			return stubContent(map[string]any{"mutation_receipt": "receipt"}), nil
		}
		if frame.Params["name"] == "read" {
			once.Do(func() { close(reading) })
			<-release
		}
		return stubContent(map[string]any{}), nil
	})
	c.options.PairedOwnACKTransactions = true
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); releaseOnce.Do(func() { close(release) }); <-done; c.editContinuations.Wait() })
	go func() {
		defer close(done)
		step := c.states[0].phase.editProbeRuntime.plan.Steps[0]
		c.callAcross(ctx, step.Request, 0, 0, 0, 0, "workload", time.Second, -1)
	}()
	awaitEditProbeSpool(t, reading)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled transaction waited forever for a read or partner")
	}
	for _, row := range ownACKTransactionSummaries(c.labels, c.records) {
		if row.Transactions != 1 || row.Outcomes["phase_censored"] != 1 || row.Latency.P95.Value != nil {
			t.Fatalf("censor population erased or given success-only p95: %+v", row)
		}
	}
}

func TestPairedOwnACKTransactionRotatesAndKeepsRPCTimings(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var c *pairedCoordinator
	c = pairedEditProbeFixture(t, ctx, 2, func(_ string, frame stubFrame) (any, *rpcError) {
		if frame.Params["name"] == "edit" {
			return stubContent(map[string]any{"mutation_receipt": "receipt"}), nil
		}
		if frame.Params["name"] == "read" {
			args, _ := frame.Params["arguments"].(map[string]any)
			target, _ := args["target"].(map[string]any)
			symbol, _ := target["symbol"].(string)
			parts := strings.Split(symbol, "::")
			marker := parts[len(parts)-1]
			var content strings.Builder
			content.WriteString("package sample\n")
			for _, step := range c.states[0].phase.editProbeRuntime.plan.Steps {
				fmt.Fprintf(&content, "func %s() int { return Example() }\n", step.Marker)
			}
			return stubContent(map[string]any{"name": marker, "source": content.String(), "content": content.String()}), nil
		}
		return stubContent(map[string]any{}), nil
	})
	c.options.PairedOwnACKTransactions = true
	for _, state := range c.states {
		for _, s := range state.phase.editProbeRuntime.states {
			zero := int64(0)
			s.record.ActivatedNS = &zero
		}
	}
	t.Cleanup(func() { cancel(); c.editContinuations.Wait() })
	for _, step := range c.states[0].phase.editProbeRuntime.plan.Steps {
		c.callAcross(ctx, step.Request, 0, 0, step.Ordinal, 0, "workload", time.Second, -1)
	}
	var transactions []pairedRecord
	for _, row := range c.records {
		if row.Tuple.Role == "transaction" {
			transactions = append(transactions, row)
		}
	}
	if len(transactions) != 2 || transactions[0].Order[0] != "baseline" || transactions[1].Order[0] != "candidate" {
		t.Fatalf("whole transaction build order did not rotate: %+v", transactions)
	}
	for _, row := range transactions {
		for _, s := range row.Samples {
			if s.Outcome != "success" || s.Transaction == nil || len(s.Transaction.Reads) != 2 {
				t.Fatalf("exact file/source transaction lost: %+v", s)
			}
			d := s.Transaction
			if d.ACKLatency.Value == nil || d.OwnACKDispatchDelay.Value == nil || d.Reads[0].Kind != "file" || d.Reads[1].Kind != "source" || d.Reads[0].Started.Before(d.ACKFinished) || d.Reads[1].Started.Before(d.Reads[0].Ended) {
				t.Fatalf("ACK/RPC/dispatch endpoints confused: %+v", d)
			}
			if d.Reads[0].Latency.Value == nil || d.Reads[1].Latency.Value == nil || s.Latency.Value == nil || *s.Latency.Value < *d.Reads[0].Latency.Value {
				t.Fatal("whole transaction relabeled as individual RPC")
			}
		}
	}
}

func TestPairedOwnACKTransactionFileDispatchUsesFirstAttempt(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	first := make(map[string]time.Time)
	c := pairedEditProbeFixture(t, ctx, 1, func(label string, frame stubFrame) (any, *rpcError) {
		if frame.Params["name"] == "edit" {
			return stubContent(map[string]any{"mutation_receipt": "receipt"}), nil
		}
		args, _ := frame.Params["arguments"].(map[string]any)
		if frame.Params["name"] == "read" && args["operation"] == "file" {
			mu.Lock()
			at := first[label]
			if at.IsZero() {
				first[label] = time.Now()
			}
			mu.Unlock()
			if at.IsZero() {
				return nil, &rpcError{Code: -32002, Message: "busy"}
			}
		}
		return stubContent(map[string]any{}), nil
	})
	c.options.PairedOwnACKTransactions = true
	t.Cleanup(func() { cancel(); c.editContinuations.Wait() })
	step := c.states[0].phase.editProbeRuntime.plan.Steps[0]
	c.callAcross(ctx, step.Request, 0, 0, 0, 0, "workload", time.Second, -1)
	for _, record := range c.records {
		if record.Tuple.Role == "transaction" {
			for label, s := range record.Samples {
				if s.Transaction == nil || s.Transaction.OwnACKDispatchDelay.Value == nil {
					t.Fatal("first dispatch endpoint missing")
				}
				d := s.Transaction
				observed := d.ACKFinished.Add(time.Duration(*d.OwnACKDispatchDelay.Value * float64(time.Millisecond)))
				mu.Lock()
				received := first[label]
				mu.Unlock()
				if received.IsZero() || observed.After(received) {
					t.Fatalf("own-ACK handoff used retry dispatch instead of first wire request: %s", label)
				}
				file := d.Reads[0]
				if file.WireAttempts != 2 || file.FirstWriteStarted.IsZero() || !file.FirstWriteStarted.Before(file.WriteStarted) || file.Latency.Value == nil || file.FinalAttemptLatency.Value == nil || *file.Latency.Value <= *file.FinalAttemptLatency.Value {
					t.Fatalf("retry/backoff or first/final wire endpoints lost: %+v", file)
				}
			}
		}
	}
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A settled oracle that keeps answering the same correct bytes is folded; its
// first answer, the immediate readbacks and receipt status stay in full, and
// the burst fits the unchanged per-edit cap that it would otherwise exceed.
func TestEditProbeRepeatFoldKeepsBurstUnderRawCap(t *testing.T) {
	const burst = 240
	for _, lossless := range []bool{false, true} {
		t.Run(fmt.Sprintf("lossless=%t", lossless), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			corpus := editProbeCorpus()
			plan, err := makeEditProbePlan(corpus, 42, 1)
			if err != nil {
				t.Fatal(err)
			}
			step := plan.Steps[0]
			root := t.TempDir()
			original := "package fixture\nfunc Example() int { return 1 }\n" + corpus.EditAnchor + "\n"
			match, _ := step.Request.Arguments["match"].(string)
			replacement, _ := step.Request.Arguments["replacement"].(string)
			if match == "" || strings.Count(original, match) != 1 {
				t.Fatal("fixture edit match missing")
			}
			// The fixture already carries the edit, so every probe answer is correct.
			if err := os.WriteFile(filepath.Join(root, corpus.File), []byte(strings.Replace(original, match, replacement, 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			disk := editProbeDiskReplies(corpus, root, func(err error) { t.Errorf("fixture read: %v", err) })
			var variant atomic.Int64
			run := newEditProbeStubRun(t, ctx, corpus, plan, root, 1, func(frame stubFrame) (any, *rpcError) {
				if frame.Params["name"] == "change" {
					return stubContent(map[string]any{"mutation_id": step.MutationID, "graph_status_terminal": true, "graph_status": "fresh"}), nil
				}
				reply, rpcErr := disk(frame)
				return editProbePaddedReply(t, reply, fmt.Sprintf("%d:%s", variant.Load(), strings.Repeat("p", 96<<10))), rpcErr
			})
			s := run.states[0]
			s.losslessEvidence = lossless
			oracle := editProbeOracles(corpus, step)[0]
			s.mu.Lock()
			s.started = time.Now().Add(-time.Second)
			s.acknowledged = s.started.Add(time.Millisecond)
			s.record.GraphTerminal = true
			s.mu.Unlock()
			for range burst {
				if err := run.probeOnce(ctx, s, oracle, 10); err != nil {
					t.Fatal(err)
				}
			}
			// A different answer ends the run and is kept in full after it.
			variant.Store(1)
			if err := run.probeOnce(ctx, s, oracle, 10); err != nil {
				t.Fatal(err)
			}
			// Immediate readbacks and receipt status repeat the same bytes and are never folded.
			readback := editProbeReadbackRequest(corpus.File, step, "source")
			status := request{Tool: "change", Operation: "receipt", Arguments: map[string]any{"operation": "receipt", "options": map[string]any{"mutation_id": step.MutationID}}}
			for range 3 {
				if _, _, err := run.perform(ctx, s, s.client, readback, "readback:source", "readback", 100); err != nil {
					t.Fatal(err)
				}
				if _, _, err := run.perform(ctx, s, s.client, status, "status", "probe", 200); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.flushRepeats(); err != nil {
				t.Fatal(err)
			}
			s.finishRaw()

			s.mu.Lock()
			firstCorrect := s.record.FirstCorrect[oracle.name]
			s.mu.Unlock()
			if firstCorrect.Value == nil {
				t.Fatal("the first answer did not record first-correct")
			}
			s.sinkMu.Lock()
			accepted, sinkErr := s.rawAccepted, s.sinkErr
			s.sinkMu.Unlock()
			if sinkErr != nil || accepted >= editProbeRawLimit {
				t.Fatalf("burst did not fit the per-edit cap: accepted=%d err=%v", accepted, sinkErr)
			}
			if got := s.foldedRepeats(); got != burst-1 {
				t.Fatalf("folded=%d, want %d", got, burst-1)
			}
			if len(run.phase.Attempts) != burst+1+6 {
				t.Fatalf("attempts=%d: folding must not hide calls from the record", len(run.phase.Attempts))
			}

			sink := s.sink.(*editProbeHeldSink)
			sink.mu.Lock()
			spool := bytes.Clone(sink.data.Bytes())
			sink.mu.Unlock()
			var history editProbePayloadHistory
			var order []string
			payloads := map[string][][]byte{}
			var fold map[string]json.RawMessage
			lines := bufio.NewScanner(bytes.NewReader(spool))
			lines.Buffer(nil, editProbeRawLimit)
			for lines.Scan() {
				var row map[string]json.RawMessage
				if err := json.Unmarshal(lines.Bytes(), &row); err != nil {
					t.Fatal(err)
				}
				event, purpose := spoolString(row, "event"), spoolString(row, "purpose")
				if purpose == "" {
					purpose = "oracle:" + spoolString(row, "oracle")
				}
				order = append(order, event+"/"+purpose)
				if event == "repeat_fold" {
					fold = row
				}
				if _, present := row["payload"]; present {
					payload, err := history.decode(row)
					if err != nil {
						t.Fatal(err)
					}
					payloads[purpose] = append(payloads[purpose], payload)
				}
			}
			if err := lines.Err(); err != nil {
				t.Fatal(err)
			}
			purpose := "oracle:" + oracle.name
			want := []string{"rpc/" + purpose, "logical_call/" + purpose, "probe/" + purpose, "repeat_fold/" + purpose, "rpc/" + purpose, "logical_call/" + purpose, "probe/" + purpose}
			for range 3 {
				want = append(want, "rpc/readback:source", "logical_call/readback:source", "rpc/status", "logical_call/status")
			}
			if strings.Join(order, " ") != strings.Join(want, " ") {
				t.Fatalf("spool rows:\n got %v\nwant %v", order, want)
			}
			first := payloads[purpose][0]
			if len(payloads[purpose]) != 2 || bytes.Equal(first, payloads[purpose][1]) || len(payloads["readback:source"]) != 3 || len(payloads["status"]) != 3 {
				t.Fatalf("full payloads: oracle=%d readback=%d status=%d", len(payloads[purpose]), len(payloads["readback:source"]), len(payloads["status"]))
			}
			if !lossless && burst*len(first) <= editProbeRawLimit {
				t.Fatalf("burst of %d-byte answers would not have exceeded the cap", len(first))
			}
			var count int
			var digest string
			if json.Unmarshal(fold["count"], &count) != nil || count != burst-1 || json.Unmarshal(fold["payload_sha256"], &digest) != nil || digest != fmt.Sprintf("%x", sha256.Sum256(first)) ||
				string(fold["correct"]) != "true" || string(fold["first_wire_id"]) == "null" || string(fold["last_end_ns"]) == "null" {
				t.Fatalf("repeat_fold row does not account for the folded answers: %s", spoolRow(fold))
			}
		})
	}
}

func editProbePaddedReply(t *testing.T, reply any, padding string) any {
	t.Helper()
	envelope, _ := reply.(map[string]any)
	content, _ := envelope["content"].([]any)
	if len(content) != 1 {
		return reply
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	var body map[string]any
	if json.Unmarshal([]byte(text), &body) != nil {
		return reply
	}
	body["padding"] = padding
	return stubContent(body)
}

func spoolString(row map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(row[key], &value)
	return value
}

func spoolRow(row map[string]json.RawMessage) string {
	data, _ := json.Marshal(row)
	return string(data)
}

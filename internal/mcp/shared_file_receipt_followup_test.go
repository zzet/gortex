package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

type sharedReceiptIdentity struct {
	Receipt   string `json:"receipt"`
	Ticket    string `json:"ticket"`
	Path      string `json:"path"`
	SHA       string `json:"content_sha256"`
	Commit    uint64 `json:"commit_seq"`
	Requested uint64 `json:"requested_generation"`
	Applied   uint64 `json:"applied_generation"`
}

type sharedReceiptFollowup struct {
	Original       sharedReceiptIdentity `json:"original_receipt"`
	Reason         string                `json:"first_refusal_reason"`
	SelectedSource uint64                `json:"selected_source_seq,omitempty"`
	Successor      uint64                `json:"newer_physical_commit_seq,omitempty"`
	FirstWaitMeta  json.RawMessage       `json:"first_watcher_wait_metadata,omitempty"`
	Status         json.RawMessage       `json:"later_mutation_status,omitempty"`
	Response       json.RawMessage       `json:"later_caller_response,omitempty"`
	PhysicalSHA256 string                `json:"later_physical_sha256,omitempty"`
	ExpectedLine   int                   `json:"expected_line,omitempty"`
	CaughtUp       []uint64              `json:"caught_up_generations,omitempty"`
	Elapsed        time.Duration         `json:"observation_elapsed_ns"`
	Error          string                `json:"error,omitempty"`
	Verified       bool                  `json:"verified"`
}

type sharedCallerPredicate struct {
	Attempt        int             `json:"attempt"`
	Site           string          `json:"site"`
	Decision       string          `json:"decision"`
	SelectedID     string          `json:"selected_id"`
	SelectedCommit uint64          `json:"selected_seq"`
	SelectedSeq    uint64          `json:"selected_source_seq"`
	CurrentID      string          `json:"current_id"`
	CurrentSeq     uint64          `json:"current_seq"`
	CurrentSource  uint64          `json:"current_source_seq"`
	Shared         json.RawMessage `json:"shared_remaining_ns"`
	Original       json.RawMessage `json:"original_remaining_ns"`
}

type sharedReceiptResponse struct {
	IsError bool `json:"isError"`
	Content []struct {
		Type, Text string
	} `json:"content"`
}

type sharedCallerEnvelope struct {
	sharedReceiptResponse
	Meta struct {
		Wait  json.RawMessage `json:"freshness"`
		Trace struct {
			First    sharedCallerPredicate   `json:"first_refusal"`
			Recovery []sharedCallerPredicate `json:"recovery_samples"`
			Overflow bool                    `json:"overflow"`
		} `json:"gortex_projection_predicate"`
	} `json:"_meta"`
}

func sharedReceiptCallerError(raw json.RawMessage) bool {
	var reply struct {
		IsError bool `json:"isError"`
	}
	return json.Unmarshal(raw, &reply) == nil && reply.IsError
}

// Identify only the two approved refusal predicates. The MCP error must carry
// no graph payload. The earlier watcher-wait metadata is a separate witness.
func sharedReceiptRefusal(raw json.RawMessage) (reason string, selected, successor uint64, wait json.RawMessage) {
	var reply sharedCallerEnvelope
	if json.Unmarshal(raw, &reply) != nil || !reply.IsError || len(reply.Content) != 1 ||
		reply.Content[0].Type != "text" || reply.Meta.Trace.Overflow {
		return "", 0, 0, nil
	}
	switch reply.Content[0].Text {
	case "projection not published: superseded", "relations: pending (1 files)\nprojection not published: superseded":
	default:
		return "", 0, 0, nil
	}
	var route struct {
		GraphFreshness string `json:"graph_freshness"`
	}
	if len(reply.Meta.Wait) > 0 && (json.Unmarshal(reply.Meta.Wait, &route) != nil || route.GraphFreshness == "fresh") {
		return "", 0, 0, nil
	}
	first := reply.Meta.Trace.First
	if first.Attempt != 1 {
		return "", 0, 0, nil
	}
	newerPhysical := first.CurrentID != "" && first.CurrentID != "nil" && first.CurrentID != "UNOBSERVED" &&
		first.CurrentSeq == first.CurrentSource && first.CurrentSeq > first.SelectedSeq
	// The read-built publisher also fences against an observed empty slot.
	// Its actual nil-to-physical CAS failure has no selected source receipt.
	// The caller must instead prove the newer physical row against its own ACK.
	if first.Site == "publish.expected_owner" && first.Decision == "owner_changed" && newerPhysical &&
		first.SelectedID == "nil" && first.SelectedCommit == 0 && first.SelectedSeq == 0 {
		return "newer_physical_owner", 0, first.CurrentSeq, reply.Meta.Wait
	}
	if first.SelectedSeq == 0 ||
		first.SelectedID == "" || first.SelectedID == "nil" || first.SelectedID == "UNOBSERVED" {
		return "", 0, 0, nil
	}
	switch first.Site {
	case "bind.retain", "bind.final_validation", "projection.validation", "response.decoration":
		if first.Decision != "owner_before" && first.Decision != "owner_after" {
			return "", 0, 0, nil
		}
	case "selection.owner_before_retain":
		if first.Decision != "owner_changed_before_retain" {
			return "", 0, 0, nil
		}
	default:
		return "", 0, 0, nil
	}
	if newerPhysical {
		return "newer_physical_owner", first.SelectedSeq, first.CurrentSeq, reply.Meta.Wait
	}
	if first.CurrentID == "nil" && len(reply.Meta.Trace.Recovery) > 0 {
		sample := reply.Meta.Trace.Recovery[0]
		var shared, original int64
		if sample.Attempt == 1 && sample.Decision == "expired" &&
			(sample.Site == "recovery.entry_budget" || sample.Site == "recovery.bind_entry_budget") &&
			json.Unmarshal(sample.Shared, &shared) == nil && json.Unmarshal(sample.Original, &original) == nil &&
			shared < 0 && original > 0 {
			return "expired_projection_allowance", first.SelectedSeq, 0, reply.Meta.Wait
		}
	}
	return "", 0, 0, nil
}

type sharedReceiptStatus struct {
	Receipt         string            `json:"receipt"`
	MutationReceipt string            `json:"mutation_receipt"`
	Ticket          string            `json:"reindex_receipt"`
	Path            string            `json:"path"`
	SHA             string            `json:"content_sha256"`
	DiskStatus      string            `json:"disk_status"`
	ACKMode         string            `json:"ack_mode"`
	Commit          uint64            `json:"commit_seq"`
	Requested       uint64            `json:"reindex_generation"`
	Applied         uint64            `json:"applied_generation"`
	Tiers           map[string]string `json:"tier_state"`
}

func sharedReceiptDecode(raw json.RawMessage, value any) bool {
	var reply sharedReceiptResponse
	return json.Unmarshal(raw, &reply) == nil && !reply.IsError && len(reply.Content) > 0 &&
		reply.Content[0].Type == "text" && json.Unmarshal([]byte(reply.Content[0].Text), value) == nil
}

func (s sharedReceiptStatus) identity() sharedReceiptIdentity {
	return sharedReceiptIdentity{s.Receipt, s.Ticket, s.Path, s.SHA, s.Commit, s.Requested, s.Applied}
}

func (f *sharedReceiptFixture) followupIdentity(row *sharedReceiptOffer) (sharedReceiptIdentity, bool) {
	var ack, status sharedReceiptStatus
	if !sharedReceiptDecode(row.content["edit_file"], &ack) || !sharedReceiptDecode(row.content["mutation_status"], &status) ||
		ack.MutationReceipt != row.receipt || status.Receipt != row.receipt || row.receipt == "" ||
		ack.Ticket == "" || ack.Ticket != status.Ticket || ack.Commit == 0 || ack.Commit != status.Commit ||
		ack.SHA == "" || ack.SHA != status.SHA || ack.Path != "main.go" || status.Path != ack.Path ||
		ack.DiskStatus != mutationDiskCommitted || status.DiskStatus != mutationDiskCommitted ||
		ack.ACKMode != "local" || status.ACKMode != "local" || ack.Requested != row.generation ||
		status.Requested != row.generation || status.Applied != row.applied || row.applied < row.generation ||
		status.Tiers["relations"] != "fresh" {
		return sharedReceiptIdentity{}, false
	}
	record, found := f.server.mutationCommits.byReceipt(row.receipt)
	if !found {
		return sharedReceiptIdentity{}, false
	}
	record.mu.RLock()
	owned := record.owner == f.server && record.isPhysicalCommit() && record.commitSeq == status.Commit &&
		pathkey.MutationKey(record.absPath) == pathkey.MutationKey(filepath.Join(f.root, status.Path))
	record.mu.RUnlock()
	return status.identity(), owned
}

func (f *sharedReceiptFixture) verifyRefusedCallers(rows []*sharedReceiptOffer) {
	candidates := false
	for _, row := range rows {
		if row.err != nil && !row.finished.IsZero() && sharedReceiptCallerError(row.content["get_callers"]) {
			candidates = true
			break
		}
	}
	if !candidates {
		return
	}
	identities := make(map[uint64]sharedReceiptIdentity, len(rows))
	originals := make(map[*sharedReceiptOffer]sharedReceiptIdentity, len(rows))
	for _, row := range rows {
		if identity, ok := f.followupIdentity(row); ok {
			if _, duplicate := identities[identity.Commit]; duplicate {
				return
			}
			identities[identity.Commit] = identity
			originals[row] = identity
		}
	}
	// The last original observer has returned. Give this independent evidence
	// phase one shared absolute deadline across all of its one-shot reads.
	end := time.Now().Add(freshnessDefaultWait)
	for _, row := range rows {
		if row.population != "loaded" || row.err == nil || row.finished.IsZero() {
			continue
		}
		identity, ok := originals[row]
		reason, selected, successor, wait := sharedReceiptRefusal(row.content["get_callers"])
		if !ok || reason == "" || selected != 0 && identities[selected].Path != identity.Path ||
			successor != 0 && (successor <= identity.Commit || identities[successor].Path != identity.Path) {
			continue
		}
		// finished is set only after the original impact passed its own exactness oracle.
		evidence := &sharedReceiptFollowup{
			Original: identity, Reason: reason, SelectedSource: selected, Successor: successor, FirstWaitMeta: wait,
		}
		row.followup = evidence
		start := time.Now()
		err := f.verifyOwnCaller(row.agent, row.ordinal, end, evidence)
		evidence.Elapsed = time.Since(start)
		if err != nil {
			evidence.Error = err.Error()
		} else {
			evidence.Verified = true
		}
	}
}

func (f *sharedReceiptFixture) verifyOwnCaller(agent, ordinal int, end time.Time, evidence *sharedReceiptFollowup) error {
	ctx, cancel := context.WithDeadline(f.contexts[agent], end)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	status, err := externalVersionRegisteredCall(f.server, ctx, 500+ordinal, "mutation_status", map[string]any{
		"receipt": evidence.Original.Receipt, "format": "json", "max_bytes": 0,
	})
	if err != nil {
		return err
	}
	var current sharedReceiptStatus
	evidence.Status, err = json.Marshal(status)
	if err != nil || !sharedReceiptDecode(evidence.Status, &current) || current.identity() != evidence.Original ||
		current.DiskStatus != mutationDiskCommitted || current.ACKMode != "local" || current.Tiers["relations"] != "fresh" {
		return fmt.Errorf("follow-up status differs from the original receipt")
	}

	path := filepath.Join(f.root, evidence.Original.Path)
	before, source, err := sharedReceiptFollowupFile(path)
	if err != nil {
		return err
	}
	want := fmt.Sprintf("Stream%dEdit%02d", agent, ordinal)
	line, err := sharedReceiptFollowupLine(source, want)
	if err != nil {
		return err
	}
	evidence.ExpectedLine = line
	watcher, ok := f.server.currentWatcher().(*sharedReceiptWatcher)
	if !ok {
		return fmt.Errorf("follow-up needs the fixture's own watcher")
	}
	// Each read starts once the watcher has no admitted generation for the
	// file. A reply that reports the file's relations pending is read again
	// only when the server's own freshness snapshots for that read saw a
	// generation admitted during it; the next reply after that generation
	// publishes is scored again. The shared deadline bounds every wait.
	var verdict error
	for {
		if err := sharedReceiptAwaitNoGeneration(ctx, watcher.Watcher, path); err != nil {
			return errors.Join(verdict, err)
		}
		admitted := watcher.relations.generation()
		reply, err := externalVersionRegisteredCall(f.server, ctx, 600+ordinal, "get_callers", sharedReceiptCallersArguments())
		if err != nil {
			return err
		}
		evidence.Response, err = json.Marshal(reply)
		if err != nil {
			return err
		}
		verdict = sharedReceiptValidateFollowup(evidence.Response, "main.go::"+want, line)
		if !errors.Is(verdict, errSharedReceiptFollowupPending) {
			break
		}
		caught := watcher.relations.generation()
		if caught <= admitted {
			return fmt.Errorf("%w, and no generation admitted during its read explains it", verdict)
		}
		evidence.CaughtUp = append(evidence.CaughtUp, caught)
	}
	after, final, err := sharedReceiptFollowupFile(path)
	if err != nil {
		return err
	}
	physicalSHA := sha256.Sum256(source)
	if !before.matches(after) || physicalSHA != sha256.Sum256(final) {
		return fmt.Errorf("physical file changed during follow-up")
	}
	evidence.PhysicalSHA256 = fmt.Sprintf("%x", physicalSHA)
	if verdict != nil {
		return verdict
	}
	return ctx.Err()
}

func sharedReceiptAwaitNoGeneration(ctx context.Context, watcher *indexer.Watcher, path string) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for watcher.TierState(path).Generation != 0 {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return fmt.Errorf("the watcher kept an admitted generation for the file: %w", ctx.Err())
		}
	}
	return nil
}

func sharedReceiptFollowupFile(path string) (diskStamp, []byte, error) {
	stamp, err := statDiskStamp(path)
	if err != nil || stamp.info == nil || !stamp.info.Mode().IsRegular() {
		return diskStamp{}, nil, fmt.Errorf("follow-up physical file unavailable: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return diskStamp{}, nil, err
	}
	defer file.Close()
	// The held fixture uses the same 8 KiB physical witness limit as the
	// earlier failure-only observer. One extra byte detects an invalid file.
	raw, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(raw) > 8192 {
		return diskStamp{}, nil, fmt.Errorf("follow-up physical witness unavailable: %v", err)
	}
	return stamp, raw, nil
}

func sharedReceiptFollowupLine(raw []byte, symbol string) (int, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", raw, parser.SkipObjectResolution)
	if err != nil {
		return 0, err
	}
	line, count := 0, 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != symbol || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if target, ok := call.Fun.(*ast.Ident); ok && target.Name == "Target" && len(call.Args) == 0 {
					line = fset.Position(call.Pos()).Line
					count++
				}
			}
			return true
		})
	}
	if count != 1 {
		return 0, fmt.Errorf("physical caller %s has %d exact target calls", symbol, count)
	}
	return line, nil
}

type sharedReceiptCallerFreshness struct {
	Graph        string   `json:"graph_freshness"`
	Scope        string   `json:"freshness_scope"`
	Complete     *bool    `json:"complete"`
	Fresh        *bool    `json:"fresh"`
	PendingCount *int     `json:"pending_file_count"`
	PendingFiles []string `json:"pending_files"`
}

// errSharedReceiptFollowupPending marks a reply whose only gap is a pending
// relations label for the follow-up's own file. It is never final evidence.
var errSharedReceiptFollowupPending = errors.New("relations pending for main.go")

func sharedReceiptValidateFollowup(raw json.RawMessage, own string, line int) error {
	var response struct {
		sharedReceiptCallerFreshness
		Freshness  sharedReceiptCallerFreshness `json:"freshness"`
		Truncated  bool                         `json:"truncated"`
		LowerBound bool                         `json:"lower_bound"`
		Edges      []struct {
			From, To, Kind string
			FilePath       string `json:"file_path"`
			Line           int
		}
	}
	if !sharedReceiptDecode(raw, &response) || response.Truncated || response.LowerBound {
		return fmt.Errorf("follow-up caller response was refused, untyped or truncated")
	}
	direct, nested := response.sharedReceiptCallerFreshness, response.Freshness
	if !direct.consistent() || !nested.consistent() || direct.Graph == "" && nested.Graph == "" ||
		direct.Graph != "" && nested.Graph != "" && direct.Graph != nested.Graph {
		if direct.pendingOwnFile() && nested.pendingOwnFile() ||
			direct.pendingOwnFile() && nested.Graph == "" && nested.consistent() ||
			nested.pendingOwnFile() && direct.Graph == "" && direct.consistent() {
			return fmt.Errorf("follow-up caller response has an unsupported relations state: %w", errSharedReceiptFollowupPending)
		}
		return fmt.Errorf("follow-up caller response has an unsupported relations state")
	}
	exact := 0
	for _, edge := range response.Edges {
		if edge.From == own && edge.To == "main.go::Target" && edge.Kind == "calls" {
			if edge.FilePath != "main.go" || edge.Line != line {
				return fmt.Errorf("follow-up own caller edge differs from the physical source")
			}
			exact++
		}
	}
	if exact != 1 {
		return fmt.Errorf("follow-up contains %d exact edges for %s", exact, own)
	}
	return nil
}

func (s sharedReceiptCallerFreshness) pendingOwnFile() bool {
	return s.Graph == "pending" && s.Scope == "relations" && s.Complete != nil && !*s.Complete &&
		s.PendingCount != nil && *s.PendingCount == 1 && slices.Equal(s.PendingFiles, []string{"main.go"})
}

func (s sharedReceiptCallerFreshness) consistent() bool {
	if s.Fresh != nil && !*s.Fresh || s.PendingCount != nil && *s.PendingCount != 0 {
		return false
	}
	if s.Graph == "unknown" {
		// This checkout has no graph-wide authority witness. Accept the
		// conservative label only with the watched wait and no pending files.
		// The original receipt and the independent physical edge still verify.
		return s.Fresh != nil && *s.Fresh && s.Complete != nil && !*s.Complete && s.PendingCount != nil
	}
	return (s.Graph == "" || s.Graph == "fresh") && (s.Complete == nil || *s.Complete)
}

// Classifier controls use fixed envelopes. The live receipt/edge test below
// separately exercises the registered status and caller reads.
func TestSharedReceiptCallerRefusalEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		edit       func(first, sample, meta map[string]any)
		selected   uint64
	}{
		{"newer_physical", "newer_physical_owner", nil, 20},
		{"newer_physical_in_expected_empty_slot", "newer_physical_owner", func(first, _, _ map[string]any) {
			first["site"], first["decision"] = "publish.expected_owner", "owner_changed"
			first["selected_id"], first["selected_seq"], first["selected_source_seq"] = "nil", 0, 0
		}, 0},
		{"publish_expected_unobserved", "", func(first, _, _ map[string]any) {
			first["site"], first["decision"] = "publish.expected_owner", "owner_changed"
			first["selected_id"], first["selected_seq"], first["selected_source_seq"] = "UNOBSERVED", 0, 0
		}, 0},
		{"expired", "expired_projection_allowance", func(first, _, _ map[string]any) {
			first["current_id"], first["current_seq"], first["current_source_seq"] = "nil", 0, 0
		}, 20},
		{"same_version_read_cow", "", func(first, _, _ map[string]any) {
			first["current_seq"], first["current_source_seq"] = 0, 20
		}, 0},
		{"changed_stamp", "", func(first, _, _ map[string]any) {
			first["decision"] = "stamp_changed"
		}, 0},
		{"suspect_before_with_later_peer", "", func(first, sample, _ map[string]any) {
			first["site"], first["decision"], first["suspect"] = "projection.validation", "suspect_before", true
			first["current_id"] = "UNOBSERVED"
			delete(first, "current_seq")
			delete(first, "current_source_seq")
			sample["site"], sample["decision"] = "recovery.checked_change", "refused"
			sample["selected_source_seq"], sample["current_id"] = 20, "new"
			sample["current_seq"], sample["current_source_seq"] = 21, 21
			sample["shared_remaining_ns"], sample["original_remaining_ns"] = 100, 200
		}, 0},
		{"unknown_nil", "", func(first, sample, _ map[string]any) {
			first["current_id"], first["current_seq"], first["current_source_seq"] = "nil", 0, 0
			sample["shared_remaining_ns"] = "UNOBSERVED"
		}, 0},
		{"caller_expired", "", func(first, sample, _ map[string]any) {
			first["current_id"], first["current_seq"], first["current_source_seq"] = "nil", 0, 0
			sample["original_remaining_ns"] = -1
		}, 0},
		{"false_fresh_tier", "", func(_, _, meta map[string]any) {
			meta["freshness"] = map[string]any{"fresh": true, "graph_freshness": "fresh"}
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := map[string]any{"attempt": 1, "site": "bind.retain", "decision": "owner_before",
				"selected_id": "old", "selected_source_seq": 20, "current_id": "new", "current_seq": 21, "current_source_seq": 21}
			sample := map[string]any{"attempt": 1, "site": "recovery.bind_entry_budget", "decision": "expired",
				"shared_remaining_ns": -10, "original_remaining_ns": 100}
			meta := map[string]any{"freshness": map[string]any{"fresh": true}}
			if tc.edit != nil {
				tc.edit(first, sample, meta)
			}
			meta["gortex_projection_predicate"] = map[string]any{"first_refusal": first, "recovery_samples": []any{sample}}
			wire := map[string]any{"isError": true, "_meta": meta,
				"content": []any{map[string]any{"type": "text", "text": "projection not published: superseded"}}}
			raw, err := json.Marshal(wire)
			require.NoError(t, err)
			got, selected, next, wait := sharedReceiptRefusal(raw)
			require.Equal(t, tc.want, got)
			if got != "" {
				require.Equal(t, tc.selected, selected)
				require.JSONEq(t, `{"fresh":true}`, string(wait), "earlier wait outcome remains a separate field")
				if got == "newer_physical_owner" {
					require.Equal(t, uint64(21), next)
				} else {
					require.Zero(t, next)
				}
			}
			wire["isError"] = false
			raw, err = json.Marshal(wire)
			require.NoError(t, err)
			got, _, _, _ = sharedReceiptRefusal(raw)
			require.Empty(t, got, "a success or wrong graph payload cannot qualify as an error")
		})
	}
}

func TestSharedReceiptFollowupVerifiesOriginalReceipt(t *testing.T) {
	selectedProjectionRegistry(t)
	t.Setenv("GORTEX_QUERY_LOG", filepath.Join(t.TempDir(), "query-log.jsonl"))
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	f := newSharedReceiptFixture(t)
	original := &sharedReceiptOffer{agent: 0, ordinal: 0, ticket: make(chan *indexer.MutationTicket, 1)}
	f.edit(original)
	f.observe(original)
	require.NoError(t, original.err)
	identity, ok := f.followupIdentity(original)
	require.True(t, ok)
	peer := &sharedReceiptOffer{agent: 1, ordinal: 0, ticket: make(chan *indexer.MutationTicket, 1)}
	f.edit(peer)
	f.observe(peer)
	require.NoError(t, peer.err)
	peerIdentity, ok := f.followupIdentity(peer)
	require.True(t, ok)
	require.Greater(t, peerIdentity.Commit, identity.Commit)
	require.NotEqual(t, peerIdentity.SHA, identity.SHA)
	// The actual registered status and caller replies still verify the older
	// receipt's own edge after the later physical write has finished.
	evidence := &sharedReceiptFollowup{Original: identity}
	require.NoError(t, f.verifyOwnCaller(original.agent, original.ordinal, time.Now().Add(freshnessDefaultWait), evidence))
	require.NotEmpty(t, evidence.PhysicalSHA256)
	require.Equal(t, peerIdentity.SHA, evidence.PhysicalSHA256)
	require.NotEqual(t, identity.SHA, evidence.PhysicalSHA256)
	require.Positive(t, evidence.ExpectedLine)
	require.ErrorContains(t, sharedReceiptValidateFollowup(evidence.Response, "main.go::Stream0Edit00", evidence.ExpectedLine+1), "differs from the physical source")
	for _, invalid := range []struct {
		freshness map[string]any
		pending   bool // read again once a generation admitted during the read explains it
	}{
		{map[string]any{"graph_freshness": "pending", "fresh": true, "complete": false, "pending_file_count": 1}, false},
		{map[string]any{"graph_freshness": "unknown", "fresh": true, "complete": true, "pending_file_count": 0}, false},
		{map[string]any{"graph_freshness": "pending", "fresh": true, "complete": false, "freshness_scope": "relations",
			"pending_file_count": 1, "pending_files": []string{"main.go"}}, true},
		{map[string]any{"graph_freshness": "failed", "fresh": true, "complete": false, "freshness_scope": "relations",
			"pending_file_count": 1, "pending_files": []string{"main.go"}}, false},
	} {
		var response sharedReceiptResponse
		require.NoError(t, json.Unmarshal(evidence.Response, &response))
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(response.Content[0].Text), &payload))
		payload["freshness"] = invalid.freshness
		reply, err := json.Marshal(payload)
		require.NoError(t, err)
		response.Content[0].Text = string(reply)
		raw, err := json.Marshal(response)
		require.NoError(t, err)
		err = sharedReceiptValidateFollowup(raw, "main.go::Stream0Edit00", evidence.ExpectedLine)
		require.ErrorContains(t, err, "unsupported relations state")
		require.Equal(t, invalid.pending, errors.Is(err, errSharedReceiptFollowupPending), "%v", invalid.freshness)
	}
	evidence.Original.Commit = peerIdentity.Commit
	require.ErrorContains(t, f.verifyOwnCaller(original.agent, original.ordinal, time.Now().Add(freshnessDefaultWait), evidence), "differs from the original receipt")
	t.Logf("real follow-up status and caller verified original commit %d with latest physical commit %d at line %d; caught-up generations %v", identity.Commit, peerIdentity.Commit, evidence.ExpectedLine, evidence.CaughtUp)
}

// Hold only this fixture file's projection invalidation notifications. The
// watcher still receives and indexes all source events and owns both tickets.
func holdSharedReceiptFixtureSourceEvents(t *testing.T, watcher *sharedReceiptWatcher, registry *pendingProjections, path string) func() {
	t.Helper()
	key := pathkey.MutationKey(path)
	var mu sync.Mutex
	held, count := true, 0
	watcher.OnSourceEvent(func(eventPath string) {
		if pathkey.MutationKey(eventPath) != key {
			registry.markSuspect(eventPath)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if held {
			count++
			return
		}
		registry.markSuspect(eventPath)
	})
	release := sync.OnceFunc(func() {
		mu.Lock()
		defer mu.Unlock()
		held = false
		for range count {
			registry.markSuspect(path)
		}
		watcher.OnSourceEvent(registry.markSuspect)
		t.Logf("restored the target projection callback and replayed %d held source notifications", count)
	})
	t.Cleanup(release)
	return release
}

// Exercise the actual registered refusal and later read for the same original
// offer. Scheduling the transition after the leaf result keeps the original
// freshness wait, deadline, attempt counter, and response checks in production.
func TestSharedReceiptRegisteredRefusalAndVerifiedFollowup(t *testing.T) {
	for _, reason := range []string{"newer_physical_owner", "expired_projection_allowance"} {
		t.Run(reason, func(t *testing.T) {
			selectedProjectionRegistry(t)
			t.Setenv("GORTEX_QUERY_LOG", filepath.Join(t.TempDir(), "query-log.jsonl"))
			t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
			t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "1")
			f := newSharedReceiptFixtureWithRetirement(t, false)
			path := filepath.Join(f.root, "main.go")
			retirement := &sourceProjectionRetirement{
				registry: pendingFileProjections,
				backend:  f.server.graph.(*projectionRetirementCoreOnlyStore).Store.(projectionStore),
				key:      pathkey.MutationKey(path),
			}
			var releaseSourceEvents func()
			if reason == "newer_physical_owner" {
				// The owner-fence witness isolates B's real physical publish
				// from native invalidation that can mark A suspect first.
				watcher, ok := f.server.currentWatcher().(*sharedReceiptWatcher)
				require.True(t, ok)
				releaseSourceEvents = holdSharedReceiptFixtureSourceEvents(t, watcher, retirement.registry, path)
			}
			original := &sharedReceiptOffer{agent: 0, ordinal: 0, population: "loaded", ticket: make(chan *indexer.MutationTicket, 1)}
			f.edit(original)
			require.NoError(t, original.err)
			ackOwner := liveEntry(pendingFileProjections, path)
			require.NotNil(t, ackOwner)
			require.Positive(t, ackOwner.sourceCommitSeq)
			var peer *sharedReceiptOffer
			var allowance *projectedSourceAllowance
			var onOwner func(func()) bool
			leaf, ok := f.server.facades.legacy("get_callers")
			require.True(t, ok)
			f.server.addTool(leaf.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				result, err := leaf.handler(ctx, req)
				if offer, _ := ctx.Value(sharedReceiptOfferKey{}).(*sharedReceiptOffer); offer != original || allowance != nil {
					return result, err
				}
				if !onOwner(func() {
					require.NoError(t, err)
					require.NotNil(t, result)
					require.False(t, result.IsError)
					require.True(t, f.server.hasRequestProjectionFact(ctx, "main.go"))
					// The real watcher can mark this physical owner suspect. A
					// checked read-built copy can retain the same ACK bytes.
					facts := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
					facts.mu.RLock()
					selected := facts.entries[ackOwner.key]
					facts.mu.RUnlock()
					require.NotNil(t, selected)
					pendingFileProjections.mu.Lock()
					current := pendingFileProjections.snap.Load().entries[ackOwner.key]
					checked := selected.readVersionChecked
					pendingFileProjections.mu.Unlock()
					require.Same(t, selected, current)
					require.Equal(t, ackOwner.sourceCommitSeq, selected.sourceCommitSeq)
					require.Equal(t, ackOwner.contentSHA256, selected.contentSHA256)
					require.True(t, ackOwner.disk.matches(selected.disk))
					if selected != ackOwner {
						require.Zero(t, selected.commitSeq)
						require.True(t, checked)
					}
					allowance, _ = ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
					require.NotNil(t, allowance)
					require.Equal(t, int32(1), allowance.attempts.Load())
					outer, bounded := ctx.Deadline()
					require.True(t, bounded)
					require.True(t, allowance.deadline.Before(outer))
					require.True(t, time.Now().Before(allowance.deadline))
					if reason == "newer_physical_owner" {
						var originalACK sharedReceiptStatus
						require.True(t, sharedReceiptDecode(original.content["edit_file"], &originalACK))
						require.Equal(t, original.receipt, originalACK.MutationReceipt)
						require.Equal(t, originalACK.Commit, selected.sourceCommitSeq)
						require.NotEmpty(t, originalACK.SHA)
						require.Equal(t, originalACK.SHA, selected.contentSHA256)
						require.False(t, selected.suspect.Load())

						peer = &sharedReceiptOffer{agent: 1, ordinal: 0, population: "loaded", ticket: make(chan *indexer.MutationTicket, 1)}
						f.edit(peer)
						require.NoError(t, peer.err)
						successor := liveEntry(pendingFileProjections, path)
						require.NotNil(t, successor)
						var peerACK sharedReceiptStatus
						require.True(t, sharedReceiptDecode(peer.content["edit_file"], &peerACK))
						require.Equal(t, peer.receipt, peerACK.MutationReceipt)
						require.Equal(t, peerACK.Commit, successor.commitSeq)
						require.Equal(t, successor.commitSeq, successor.sourceCommitSeq)
						require.Greater(t, successor.commitSeq, selected.sourceCommitSeq)
						require.NotEmpty(t, peerACK.SHA)
						require.Equal(t, peerACK.SHA, successor.contentSHA256)
						require.NotEqual(t, originalACK.SHA, peerACK.SHA)
						require.False(t, selected.suspect.Load())
						return
					}
					retirement.run()
					require.Nil(t, liveEntry(pendingFileProjections, path))
					require.True(t, selected.checkedRetired.Load())
					timer := time.NewTimer(time.Until(allowance.deadline))
					defer timer.Stop()
					<-timer.C
					require.NoError(t, ctx.Err())
				}) {
					return mcplib.NewToolResultError("controlled callback closed"), nil
				}
				return result, err
			})
			_, err := canonicalReadWithOwner(t, func(dispatch func(func()) bool) (*mcplib.CallToolResult, error) {
				onOwner = dispatch
				f.observe(original)
				return nil, nil
			})
			require.NoError(t, err)
			require.NotNil(t, allowance)
			require.Equal(t, int32(1), allowance.attempts.Load())
			first := append(json.RawMessage(nil), original.content["get_callers"]...)
			if releaseSourceEvents != nil {
				releaseSourceEvents()
			}
			t.Logf("controlled actual first caller response: %s", first)
			require.Error(t, original.err)
			require.False(t, original.callerExact)
			classified, _, _, _ := sharedReceiptRefusal(first)
			require.Equal(t, reason, classified, "original registered reply: %s", first)
			rows := []*sharedReceiptOffer{original}
			if peer != nil {
				// The newer owner remained pending for A's first response.
				// Release the test-only hold only after its real ticket settles,
				// before performing B's separate source-observation checks.
				commit, found := f.server.mutationCommits.byReceipt(peer.receipt)
				require.True(t, found)
				peerCommit := commit.snapshot()
				value, found := f.server.mutationReceipts.Load(peerCommit.publicationReceipt)
				require.True(t, found)
				publication := value.(*mutationReceipt)
				wait := time.NewTimer(freshnessDefaultWait)
				defer wait.Stop()
				select {
				case <-publication.done:
				case <-wait.C:
					t.Fatal("the actual peer publication did not finish")
				}
				peerOwner := liveEntry(pendingFileProjections, path)
				require.NotNil(t, peerOwner)
				require.Equal(t, peerCommit.CommitSeq, peerOwner.sourceCommitSeq)
				require.Equal(t, 1, retirement.registry.retirePaths(retirement.backend, []string{path}))
				require.Nil(t, liveEntry(pendingFileProjections, path), "the peer's published path must leave the projection cache")
				t.Logf("settled peer projection retired with replay authority=%v", peerOwner.checkedRetired.Load())
				peerEdges := f.server.graph.GetOutEdges("main.go::Stream1Edit00")
				require.Len(t, peerEdges, 1)
				require.Equal(t, "main.go::Target", peerEdges[0].To)
				require.Equal(t, "calls", string(peerEdges[0].Kind))
				f.observe(peer)
				require.NoError(t, peer.err, "peer's original caller envelope: %s", peer.content["get_callers"])
				rows = append(rows, peer)
			}
			require.Nil(t, liveEntry(pendingFileProjections, path))
			f.verifyRefusedCallers(rows)
			require.Equal(t, first, original.content["get_callers"])
			require.Error(t, original.err)
			require.NotNil(t, original.followup)
			later, err := json.Marshal(original.followup)
			require.NoError(t, err)
			t.Logf("controlled original caller follow-up: %s", later)
			require.True(t, original.followup.Verified, "%+v", original.followup)
			require.Equal(t, original.receipt, original.followup.Original.Receipt)
		})
	}
}

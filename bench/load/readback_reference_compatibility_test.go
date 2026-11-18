package main

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadbackCompatibilityNegotiatesAllDeferBeforeAnyCellCall(t *testing.T) {
	dir := shortPrivateSocketBase(t)
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	handshake := make(chan map[string]any, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		decoder, encoder := json.NewDecoder(conn), json.NewEncoder(conn)
		var hello map[string]any
		if decoder.Decode(&hello) != nil {
			return
		}
		handshake <- hello
		if encoder.Encode(map[string]any{"ok": true}) != nil {
			return
		}
		for {
			var frame struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params map[string]any  `json:"params"`
			}
			if decoder.Decode(&frame) != nil {
				return
			}
			if len(frame.ID) == 0 {
				continue
			}
			var result any = map[string]any{}
			if frame.Method == "tools/call" {
				args, _ := frame.Params["arguments"].(map[string]any)
				result = json.RawMessage(readbackReferenceWire(map[string]any{"tool": args["tool"], "enabled": true, "status": "deferred"}))
			}
			if encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": result}) != nil {
				return
			}
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	c, _, err := openReadbackReferenceCompatibility(ctx, socket, "/owned/primary", "edit_symbol")
	require.NoError(t, err)
	require.NoError(t, c.Close())
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("owned connection did not close")
	}
	hello := <-handshake
	require.Equal(t, "all", hello["tools"])
	require.Equal(t, "defer", hello["tools_mode"])
	require.Equal(t, "/owned/primary", hello["cwd"])
}

func TestReadbackCompatibilityRefusesMissingOrForeignToolAvailability(t *testing.T) {
	for _, bad := range []map[string]any{{"tool": "get_symbol", "enabled": true, "status": "blocked"}, {"tool": "foreign", "enabled": true, "status": "live"}, {"tool": "edit_symbol", "status": "live"}} {
		socket := startStub(t, func(f stubFrame) (any, *rpcError) {
			args, _ := f.Params["arguments"].(map[string]any)
			if args["tool"] != "get_symbol" {
				return json.RawMessage(readbackReferenceWire(map[string]any{"tool": args["tool"], "enabled": true, "status": "live"})), nil
			}
			return json.RawMessage(readbackReferenceWire(bad)), nil
		})
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		c, _, err := openReadbackReferenceCompatibility(ctx, socket, "/owned/primary", "edit_symbol")
		cancel()
		if c != nil {
			_ = c.Close()
		}
		require.Error(t, err, "negotiation alone cannot invent callable metadata/edit tools")
	}
}

func TestReadbackCompatibilityRequestsUseExactLegacyToolsAndCheckout(t *testing.T) {
	repo := privateRepo{Path: "/owned/primary", Prefix: "owned"}
	want := map[string]string{"add_intrafile_call": "edit_file", "append_leaf": "write_file", "modify_body": "edit_symbol", "modify_caller_body": "batch_edit", "change_signature": "batch_edit", "add_crossfile_call": "edit_file", "rename": "rename_symbol", "delete": "safe_delete_symbol", "relocate": "move_symbol"}
	for kind, name := range want {
		t.Run(kind, func(t *testing.T) {
			trial, err := readbackReferenceTrialFor(readbackReferenceCell{Agents: 1, Kind: kind, Tool: readbackReferenceTools[kind][0], Offers: 100}, editProbeArrival{}, 0)
			require.NoError(t, err)
			params, err := readbackReferenceCompatibilityParams(trial.Request, repo)
			require.NoError(t, err)
			require.Equal(t, name, params["name"])
			args := params["arguments"].(map[string]any)
			for _, field := range []string{"operation", "target", "options", "output", "match", "replacement", "changes"} {
				require.NotContains(t, args, field, "no facade envelope may be silently ignored")
			}
			switch name {
			case "batch_edit":
				require.Equal(t, trial.TransactionID, args["transaction_id"])
				items := args["edits"].([]any)
				require.Len(t, items, 1)
				require.Equal(t, "owned/"+trial.ProbeFile, items[0].(map[string]any)["path"])
			case "edit_file", "write_file":
				require.Equal(t, "owned/"+trial.ProbeFile, args["path"])
			default:
				require.Contains(t, args["id"], "owned/zz_gortex_readback_probe/")
			}
			if name == "move_symbol" {
				require.Equal(t, "owned/"+trial.ProbeFile, args["target_file"])
			}
		})
	}
	for _, req := range []request{{Tool: "read", Operation: "symbol", Arguments: map[string]any{}}, {Tool: "unknown", Arguments: map[string]any{}}, {Tool: "read", Operation: "source", Arguments: map[string]any{}}} {
		_, err := readbackReferenceCompatibilityParams(req, repo)
		require.Error(t, err, "unknown aliases or missing targets cannot fall back to a different endpoint")
	}
}

func runCompatibilityObservation(t *testing.T, metadata any) (readbackReferenceRecord, []stubFrame) {
	t.Helper()
	image, bodies := readbackReferenceOracleFixture(t)
	var mu sync.Mutex
	var frames []stubFrame
	socket := startStub(t, func(f stubFrame) (any, *rpcError) {
		mu.Lock()
		frames = append(frames, f)
		mu.Unlock()
		name, _ := f.Params["name"].(string)
		args, _ := f.Params["arguments"].(map[string]any)
		if name == "tool_profile" {
			return json.RawMessage(readbackReferenceWire(map[string]any{"tool": args["tool"], "enabled": true, "status": "live"})), nil
		}
		var body any
		switch name {
		case "edit_symbol":
			body = map[string]any{"disk_status": "committed", "ack_mode": "local", "content_sha256": readbackReferenceSHA(image.Bytes)}
		case "get_symbol":
			body = metadata
		case "get_symbol_source":
			body = bodies["source"]
		case "get_file_summary":
			body = bodies["summary"]
		case "get_editing_context":
			body = bodies["editing_context"]
		case "read_file":
			body = bodies["file"]
		default:
			return nil, &rpcError{Code: -32601, Message: "unexpected non-legacy tool " + name}
		}
		return json.RawMessage(readbackReferenceWire(body)), nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	client, profile, err := openReadbackReferenceCompatibility(ctx, socket, "/owned/primary", "edit_symbol")
	require.NoError(t, err)
	defer client.Close()
	require.Len(t, profile, 6, "real per-tool availability checked before offers")
	trial := readbackReferenceTrial{ProbeFile: image.File, Target: "Target", After: map[string]readbackReferenceImage{image.File: image}, Request: request{Tool: "edit", Operation: "symbol", Arguments: map[string]any{"operation": "symbol", "target": map[string]any{"symbol": image.File + "::Target"}, "match": "old", "replacement": "new"}}}
	row := observeReadbackReferenceCompatibility(ctx, client, privateRepo{Path: "/owned/primary", Prefix: "owned"}, trial, "under_edit", time.Second)
	mu.Lock()
	frames = append([]stubFrame(nil), frames...)
	mu.Unlock()
	return row, frames
}

func TestReadbackCompatibilityOneConnectionCallsFullMetadataAfterItsOwnEdit(t *testing.T) {
	_, bodies := readbackReferenceOracleFixture(t)
	node := bodies["summary"]["nodes"].([]any)[0]
	row, frames := runCompatibilityObservation(t, map[string]any{"node": node, "in_edges": nil, "out_edges": []any{}})
	require.True(t, row.Correct, "%+v", row.Errors)
	require.True(t, row.Probes["symbol"].Correct)
	require.NotNil(t, row.Probes["symbol"].Latency.Value)
	var params map[string]any
	require.NoError(t, json.Unmarshal(row.Probes["symbol"].Request, &params))
	require.Equal(t, "get_symbol", params["name"])
	require.Equal(t, "full", params["arguments"].(map[string]any)["detail"])
	require.Equal(t, "owned/probe.go::Target", params["arguments"].(map[string]any)["id"])
	var offered []stubFrame
	for _, f := range frames {
		require.Equal(t, 1, f.Connection, "no hidden second session")
		require.Equal(t, "/owned/primary", f.Cwd)
		if f.Params["name"] != "tool_profile" {
			offered = append(offered, f)
		}
	}
	require.Len(t, offered, 6)
	require.Equal(t, "edit_symbol", offered[0].Params["name"])
	for _, probe := range row.Probes {
		require.Greater(t, probe.ID, row.ACK.ID)
	}
}

func TestReadbackCompatibilityDoesNotCountSourceBriefOrForeignMetadata(t *testing.T) {
	_, bodies := readbackReferenceOracleFixture(t)
	node := bodies["summary"]["nodes"].([]any)[0].(map[string]any)
	foreign := copyJSON(node)
	foreign["id"] = "foreign/probe.go::Target"
	stale := copyJSON(node)
	stale["start_line"] = 1
	for name, body := range map[string]any{
		"source": bodies["source"], "brief": node,
		"foreign":       map[string]any{"node": foreign, "in_edges": nil, "out_edges": nil},
		"old_range":     map[string]any{"node": stale, "in_edges": nil, "out_edges": nil},
		"missing_edges": map[string]any{"node": node},
	} {
		t.Run(name, func(t *testing.T) {
			row, _ := runCompatibilityObservation(t, body)
			require.True(t, readbackReferenceQuartetCorrect(row.Probes), "valid quartet remains separate")
			require.False(t, row.Probes["symbol"].Correct)
			require.False(t, row.Correct)
			require.NotEmpty(t, row.Errors)
			require.Nil(t, readbackReferenceTiming([]readbackReferenceRecord{row}, "symbol", 1).P95.Value)
		})
	}
}

func TestReadbackCompatibilityBaselineCannotBorrowFacadeTiming(t *testing.T) {
	id := readbackReferenceComparisonIdentity{BinarySHA256: "b", ManifestSHA256: "m", HostSHA256: "h", PopulationSHA256: "p", ReadPlanSHA256: "r", ArrivalSHA256: "a", Agents: 8, Kind: "modify_body", ToolSurface: readbackReferenceCompatibilitySurface}
	base := id
	base.ToolSurface = readbackReferenceFacadeSurface
	require.False(t, readbackReferenceBaselineEqual(id, base))
	base.ToolSurface = ""
	require.False(t, readbackReferenceBaselineEqual(id, base))
	require.True(t, readbackReferenceBaselineEqual(id, id))
	_, err := readbackReferenceSurface("all/hide")
	require.Error(t, err, "unknown surface cannot silently widen or downgrade the session")
}

func TestReadbackCompatibilityPlanPinsOneSurfaceWithoutAddingOffers(t *testing.T) {
	root := shortPrivateSocketBase(t)
	p := readbackReferencePlan{Version: 1, Binary: filepath.Join(root, "candidate"), BinarySHA256: "0123456789012345678901234567890123456789012345678901234567890123", BinaryRevision: "0123456789012345678901234567890123456789", Manifest: filepath.Join(root, "manifest.json"), Scratch: filepath.Join(root, "scratch"), Output: filepath.Join(root, "result"), Seed: 1, EditMeanNS: int64(15 * time.Second), ProductDeadlineSeconds: 120, WarmSeconds: 1800, StopSeconds: 30, ToolSurface: readbackReferenceCompatibilitySurface}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	parsed, err := decodeReadbackReferencePlan(raw)
	require.NoError(t, err)
	require.Equal(t, readbackReferenceCompatibilitySurface, parsed.ToolSurface)
	cells, err := readbackReferenceSchedules(parsed)
	require.NoError(t, err)
	require.Len(t, cells, 90, "surface is one declared cell policy, not another cross product")
	for _, cell := range cells {
		require.Equal(t, 100, cell.Cell.Offers)
	}
	p.ToolSurface = "all/hide"
	raw, _ = json.Marshal(p)
	_, err = decodeReadbackReferencePlan(raw)
	require.Error(t, err)
}

func TestReadbackCompatibilityWarmupUsesItsExistingConnectionAndLegacyVocabulary(t *testing.T) {
	var mu sync.Mutex
	var frames []stubFrame
	socket := startStub(t, func(f stubFrame) (any, *rpcError) {
		mu.Lock()
		frames = append(frames, f)
		mu.Unlock()
		if f.Method == "resources/read" {
			return map[string]any{"contents": []any{map[string]any{"text": `{"returned":0,"total":0,"communities":[]}`}}}, nil
		}
		name, _ := f.Params["name"].(string)
		args, _ := f.Params["arguments"].(map[string]any)
		if name == "tool_profile" {
			return json.RawMessage(readbackReferenceWire(map[string]any{"tool": args["tool"], "enabled": true, "status": "deferred"})), nil
		}
		if name == "analyze" {
			return json.RawMessage(readbackReferenceWire(map[string]any{"hotspots": []any{}})), nil
		}
		return nil, &rpcError{Code: -32601, Message: "unexpected warmup operation"}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	c, _, err := openReadbackReferenceCompatibility(ctx, socket, "/owned/primary", "edit_symbol")
	require.NoError(t, err)
	defer c.Close()
	proof, err := warmReadbackReferenceCompatibility(ctx, c, time.Second)
	require.NoError(t, err)
	require.Len(t, proof, 2)
	mu.Lock()
	defer mu.Unlock()
	for _, f := range frames {
		require.Equal(t, 1, f.Connection)
		if f.Params["name"] == "analyze" {
			args := f.Params["arguments"].(map[string]any)
			require.Equal(t, "hotspots", args["kind"])
			require.NotContains(t, args, "operation")
			require.NotContains(t, args, "output")
		}
	}
}

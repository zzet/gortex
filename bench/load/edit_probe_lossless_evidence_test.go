package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestEditProbeLosslessEvidenceRetainsEveryLargePayload(t *testing.T) {
	payload := json.RawMessage(`{"source":"` + strings.Repeat("unchanged exact source body\\n", 12000) + `","pending":true,"error":"original refusal"}`)
	var sink bytes.Buffer
	state := &editProbeState{losslessEvidence: true, sink: losslessTestSink{&sink}}
	state.startRaw()
	t.Cleanup(state.finishRaw)
	for i := 0; i < 80; i++ {
		if err := state.emit(map[string]any{"event": "rpc", "purpose": "oracle:summary", "wire_id": i + 1, "call_end_ns": i + 7, "error": "original refusal", "payload": payload}); err != nil {
			t.Fatal(err)
		}
	}
	state.finishRaw()
	scanner := bufio.NewScanner(bytes.NewReader(sink.Bytes()))
	scanner.Buffer(make([]byte, 4096), editProbeRawLimit)
	count := 0
	decoder := &editProbePayloadHistory{}
	for scanner.Scan() {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		decoded, err := decoder.decode(row)
		if err != nil || !bytes.Equal(decoded, payload) {
			t.Fatalf("payload%d changed: %v", count, err)
		}
		var wire int
		var originalError string
		if err := json.Unmarshal(row["wire_id"], &wire); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(row["error"], &originalError); err != nil {
			t.Fatal(err)
		}
		if wire != count+1 || originalError != "original refusal" {
			t.Fatal("wire identity/error changed")
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 80 || state.rawBytes != sink.Len() || state.rawAccepted != state.rawBytes || state.rawBytes > editProbeRawLimit || state.rawLogicalBytes <= editProbeRawLimit {
		t.Fatalf("incomplete or unbounded capture count=%d physical=%d logical=%d", count, state.rawBytes, state.rawLogicalBytes)
	}
}

type losslessTestSink struct{ io.Writer }

func (losslessTestSink) Close() error { return nil }

func TestEditProbeLosslessEvidenceDefaultAndSingleEventBounds(t *testing.T) {
	payload := json.RawMessage(`{"body":"` + strings.Repeat("x", editProbeRawLimit) + `"}`)
	for _, optin := range []bool{false, true} {
		state := &editProbeState{losslessEvidence: optin}
		err := state.emit(map[string]any{"event": "rpc", "payload": payload})
		if !errors.Is(err, errEditProbeEvidenceIncomplete) {
			t.Fatalf("single event old bound changed optin=%v err=%v", optin, err)
		}
		if state.rawAccepted != 0 || state.rawLogicalBytes != 0 {
			t.Fatal("rejected event credited")
		}
	}
	legacy := &editProbeState{}
	if err := legacy.emit(map[string]any{"payload": json.RawMessage(`{"exact":true}`)}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(legacy.rawPending, []byte("payload_codec")) {
		t.Fatal("default capture changed")
	}
	for _, bad := range []any{json.RawMessage(`{broken`)} {
		state := &editProbeState{losslessEvidence: true}
		err := state.emit(map[string]any{"payload": bad})
		if err == nil || state.rawAccepted != 0 {
			t.Fatal("invalid event credited or accepted")
		}
	}
}

func TestEditProbeLosslessEvidenceDecoderRejectsCorruptionAndUnknownCodec(t *testing.T) {
	payload := json.RawMessage(`{"body":"` + strings.Repeat("x", 100000) + `"}`)
	encoded, _, err := encodeEditProbeEvidence(map[string]any{"payload": payload}, true)
	if err != nil {
		t.Fatal(err)
	}
	var original map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &original); err != nil {
		t.Fatal(err)
	}
	var encodedText string
	if err := json.Unmarshal(original["payload"], &encodedText); err != nil {
		t.Fatal(err)
	}
	compressed, err := base64.StdEncoding.DecodeString(encodedText)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{compressed[:len(compressed)-1], append(append([]byte{}, compressed...), byte(0)), append(append([]byte{}, compressed...), compressed...)} {
		row := make(map[string]json.RawMessage, len(original))
		for k, v := range original {
			row[k] = v
		}
		value, err := json.Marshal(base64.StdEncoding.EncodeToString(bad))
		if err != nil {
			t.Fatal(err)
		}
		row["payload"] = value
		if _, err := decodeEditProbeEvidencePayload(row); err == nil {
			t.Fatal("truncated or trailing gzip stream accepted")
		}
	}
	for _, tc := range []struct {
		key   string
		value json.RawMessage
	}{
		{"payload_codec", json.RawMessage(`"unknown"`)},
		{"payload_sha256", json.RawMessage(`"` + strings.Repeat("0", 64) + `"`)},
		{"payload_original_bytes", json.RawMessage(`1`)},
		{"payload_original_bytes", json.RawMessage(fmt.Sprint(editProbeRawLimit + 1))},
		{"payload", json.RawMessage(`"broken-base64"`)},
	} {
		row := make(map[string]json.RawMessage, len(original))
		for k, v := range original {
			row[k] = v
		}
		row[tc.key] = tc.value
		if _, err := decodeEditProbeEvidencePayload(row); err == nil {
			t.Fatalf("corrupt %s accepted", tc.key)
		}
	}
}

func TestEditProbeLosslessEvidenceSmallAndEmptyBytesAreExact(t *testing.T) {
	for _, payload := range []json.RawMessage{json.RawMessage(" \n { \"exact\": true } \n"), nil} {
		encoded, _, err := encodeEditProbeEvidence(map[string]any{"event": "rpc", "payload": payload, "error": "original error"}, true)
		if err != nil {
			t.Fatal(err)
		}
		var row map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &row); err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeEditProbeEvidencePayload(row)
		if err != nil || !bytes.Equal(decoded, payload) {
			t.Fatalf("small/empty payload changed: %v", err)
		}
	}
}

func TestEditProbeLosslessHistoryBoundsAndOrdering(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 2048; i++ {
		fmt.Fprintf(&body, "%x", sha256.Sum256([]byte(fmt.Sprint(i))))
	}
	first := json.RawMessage(fmt.Sprintf(`{"body":%q}`, body.String()))
	second := bytes.Clone(first)
	if second[20] == 'a' {
		second[20] = 'b'
	} else {
		second[20] = 'a'
	}
	state := &editProbeState{losslessEvidence: true}
	for _, payload := range []json.RawMessage{first, second, second} {
		if err := state.emit(map[string]any{"event": "rpc", "purpose": "summary", "payload": payload}); err != nil {
			t.Fatal(err)
		}
	}
	lines := bytes.Split(bytes.TrimSpace(state.rawPending), []byte("\n"))
	if len(lines) != 3 {
		t.Fatal("all events required")
	}
	decoder := &editProbePayloadHistory{}
	for i, want := range []json.RawMessage{first, second, second} {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(lines[i], &row); err != nil {
			t.Fatal(err)
		}
		got, err := decoder.decode(row)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("event%d: %v", i, err)
		}
		if i > 0 {
			var codec string
			if err := json.Unmarshal(row["payload_codec"], &codec); err != nil {
				t.Fatal(err)
			}
			if i == 1 && codec != editProbeDictionaryCodec || i == 2 && codec != editProbeReferenceCodec {
				for _, level := range []int{zlib.BestSpeed, zlib.DefaultCompression} {
					compressedBytes, encodedBytes := 0, 0
					for offset := 0; offset < len(second); offset += editProbeChunkBytes {
						end := min(offset+editProbeChunkBytes, len(second))
						var buffer bytes.Buffer
						writer, err := zlib.NewWriterLevelDict(&buffer, level, first[offset:end])
						if err != nil {
							t.Fatal(err)
						}
						if _, err := writer.Write(second[offset:end]); err != nil {
							t.Fatal(err)
						}
						if err := writer.Close(); err != nil {
							t.Fatal(err)
						}
						compressedBytes += buffer.Len()
						encodedBytes += base64.StdEncoding.EncodedLen(buffer.Len())
					}
					t.Logf("PINNED_ZLIB_DIAGNOSTIC level=%d target_bytes=%d dictionary_bytes=%d compressed_chunks_bytes=%d base64_chunks_bytes=%d", level, len(second), len(first), compressedBytes, encodedBytes)
				}
				t.Fatalf("expected real reuse: event=%d codec=%s original_bytes=%d first_bytes=%d second_bytes=%d history_bytes=%d history_keys=%d encoded_row_bytes=%d", i, codec, len(want), len(first), len(second), state.payloadHistory.bytes, len(state.payloadHistory.payloads), len(lines[i]))
			}
			if i == 1 {
				var chunks []string
				if err := json.Unmarshal(row["payload"], &chunks); err != nil {
					t.Fatal(err)
				}
				compressed, err := base64.StdEncoding.DecodeString(chunks[0])
				if err != nil {
					t.Fatal(err)
				}
				broken := append(bytes.Clone(compressed), byte(0))
				chunks[0] = base64.StdEncoding.EncodeToString(broken)
				corrupt := make(map[string]json.RawMessage)
				for k, v := range row {
					corrupt[k] = v
				}
				corrupt["payload"], err = json.Marshal(chunks)
				if err != nil {
					t.Fatal(err)
				}
				seeded := &editProbePayloadHistory{}
				seeded.remember("summary", first)
				if _, err := seeded.decode(corrupt); err == nil {
					t.Fatal("trailing dictionary bytes accepted")
				}
			}
			empty := &editProbePayloadHistory{}
			if _, err := empty.decode(row); err == nil {
				t.Fatal("missing predecessor accepted")
			}
			wrong := &editProbePayloadHistory{}
			wrong.remember("summary", []byte(`{"wrong":true}`))
			if _, err := wrong.decode(row); err == nil {
				t.Fatal("wrong predecessor accepted")
			}
			row["purpose"] = json.RawMessage(`"other"`)
			if _, err := decoder.decode(row); err == nil {
				t.Fatal("foreign purpose predecessor accepted")
			}
		}
	}
	if int64(state.rawAccepted) >= state.rawLogicalBytes {
		t.Fatal("bounded reuse did not reduce repetitive capture")
	}
	bounded := &editProbePayloadHistory{}
	for i := 0; i < 200; i++ {
		bounded.remember(fmt.Sprint(i), bytes.Repeat([]byte("x"), 65536))
	}
	if bounded.bytes > editProbeHistoryLimit || len(bounded.payloads) > editProbeHistoryKeys {
		t.Fatal("history unbounded")
	}
}

func TestEditProbeLosslessStandaloneProfileValidation(t *testing.T) {
	args := []string{"--binary", "/binary", "--manifest", "/manifest", "--scratch", "/scratch", "--scenario", "edit-and-probe", "--agents", "0", "--edit-agents", "4", "--worktrees", "0", "--edit-gate-modes", "--lossless-edit-probe-evidence"}
	o, err := parseRun(args)
	if err != nil || !o.LosslessEditProbeEvidence {
		t.Fatalf("GateModes opt-in: %v", err)
	}
	for _, change := range []func(*runOptions){func(o *runOptions) { o.EditGateModes = false }, func(o *runOptions) { o.Scenario = "read" }} {
		bad := o
		change(&bad)
		if validateRunOptions(bad) == nil {
			t.Fatal("unsupported lossless profile accepted")
		}
	}
	// An explicit per-edit raw cap may only raise the default, within a bound.
	for _, mib := range []string{"16", "256"} {
		if _, err := parseRun(append(slices.Clone(args), "--edit-probe-raw-limit-mib", mib)); err != nil {
			t.Fatalf("raw cap %s MiB: %v", mib, err)
		}
	}
	for _, mib := range []string{"-1", "8", "257"} {
		if _, err := parseRun(append(slices.Clone(args), "--edit-probe-raw-limit-mib", mib)); err == nil {
			t.Fatalf("raw cap %s MiB accepted", mib)
		}
	}
	bad := o
	bad.Scenario, bad.EditGateModes, bad.LosslessEditProbeEvidence, bad.EditProbeRawLimitMiB = "read", false, false, 64
	if validateRunOptions(bad) == nil {
		t.Fatal("raw cap accepted outside edit-and-probe")
	}
}

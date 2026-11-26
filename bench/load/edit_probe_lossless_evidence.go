package main

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const editProbeLosslessCodec = "lossless-rpc-payload-v2"
const editProbeGzipPayloadCodec = "gzip-base64-exact-payload-v1"
const editProbeIdentityPayloadCodec = "base64-exact-payload-v1"
const editProbeLosslessThreshold = 64 << 10

// Only the artifact representation changes. RPC consumers continue to receive
// the original bytes. The old single-event bound and the cumulative encoded
// JSONL bound remain; logical legacy-JSONL bytes are reported separately.
func encodeEditProbeEvidence(value any, lossless bool) ([]byte, int, error) {
	original, err := json.Marshal(value)
	if err != nil {
		return nil, 0, err
	}
	logical := len(original) + 1
	if !lossless || logical > editProbeRawLimit {
		return original, logical, nil
	}
	row, ok := value.(map[string]any)
	if !ok {
		return original, logical, nil
	}
	payload, ok := row["payload"].(json.RawMessage)
	if !ok {
		return original, logical, nil
	}
	if len(payload) > editProbeRawLimit {
		return nil, 0, fmt.Errorf("%w: bounded original RPC payload exceeds16MiB", errEditProbeEvidenceIncomplete)
	}
	codec := editProbeIdentityPayloadCodec
	encoded := base64.StdEncoding.EncodeToString(payload)
	if len(payload) >= editProbeLosslessThreshold {
		var compressed bytes.Buffer
		writer, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
		if err != nil {
			return nil, 0, err
		}
		if _, err = writer.Write(payload); err != nil {
			return nil, 0, errors.Join(err, writer.Close())
		}
		if err = writer.Close(); err != nil {
			return nil, 0, err
		}
		candidate := base64.StdEncoding.EncodeToString(compressed.Bytes())
		if len(candidate) < len(encoded) {
			codec, encoded = editProbeGzipPayloadCodec, candidate
		}
	}
	copyRow := make(map[string]any, len(row)+3)
	for key, item := range row {
		copyRow[key] = item
	}
	copyRow["payload"] = encoded
	copyRow["payload_codec"] = codec
	copyRow["payload_original_bytes"] = len(payload)
	copyRow["payload_sha256"] = fmt.Sprintf("%x", sha256.Sum256(payload))
	data, err := json.Marshal(copyRow)
	if err != nil {
		return nil, 0, err
	}
	return data, logical, nil
}

// Offline consumers decode one bounded record at a time. Unknown codecs,
// concatenated/trailing streams, length mismatch and corruption are failures,
// never an unavailable or successful substitute for the original response.
func decodeEditProbeEvidencePayload(row map[string]json.RawMessage) (json.RawMessage, error) {
	encodedCodec, present := row["payload_codec"]
	if !present {
		payload := row["payload"]
		if len(payload) > editProbeRawLimit || !json.Valid(payload) {
			return nil, errors.New("invalid legacy payload")
		}
		return payload, nil
	}
	var codec string
	if err := json.Unmarshal(encodedCodec, &codec); err != nil || (codec != editProbeGzipPayloadCodec && codec != editProbeIdentityPayloadCodec) {
		return nil, errors.New("unknown evidence payload codec")
	}
	var sizePointer *int
	if err := json.Unmarshal(row["payload_original_bytes"], &sizePointer); err != nil || sizePointer == nil || *sizePointer < 0 || *sizePointer > editProbeRawLimit {
		return nil, errors.New("invalid bounded payload length")
	}
	size := *sizePointer
	var digest, encoded string
	if err := json.Unmarshal(row["payload_sha256"], &digest); err != nil {
		return nil, err
	}
	if raw, err := hex.DecodeString(digest); err != nil || len(raw) != sha256.Size {
		return nil, errors.New("invalid payload SHA256")
	}
	if err := json.Unmarshal(row["payload"], &encoded); err != nil || len(encoded) > editProbeRawLimit {
		return nil, errors.New("invalid bounded encoded payload")
	}
	compressed, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	var payload []byte
	if codec == editProbeIdentityPayloadCodec {
		payload = compressed
	} else {
		input := bytes.NewReader(compressed)
		reader, err := gzip.NewReader(input)
		if err != nil {
			return nil, err
		}
		reader.Multistream(false)
		var readErr error
		payload, readErr = io.ReadAll(io.LimitReader(reader, int64(size)+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			return nil, errors.Join(readErr, closeErr)
		}
		if input.Len() != 0 {
			return nil, errors.New("trailing compressed payload")
		}
	}
	if len(payload) != size || fmt.Sprintf("%x", sha256.Sum256(payload)) != digest {
		return nil, errors.New("incomplete or corrupt original payload")
	}
	return json.RawMessage(payload), nil
}

const editProbeDictionaryCodec = "zlib-16k-chunks-prior-payload-v1"
const editProbeReferenceCodec = "prior-exact-payload-v1"
const editProbeHistoryLimit = 4 << 20
const editProbeHistoryKeys = 128

// Keep the corresponding predecessor strictly inside the pinned flate level-1
// match distance. A full 32 KiB chunk sits at its excluded boundary.
const editProbeChunkBytes = 16 << 10

// History is owned by one edit and serialized in accepted JSONL order. Small
// pending replies cannot evict the last large response for the same purpose.
// A deterministic clear keeps encoder and decoder bounded identically.
type editProbePayloadHistory struct {
	payloads map[string][]byte
	bytes    int
}

func editProbeHistoryKey(purpose string, payload []byte) string {
	if len(payload) >= editProbeLosslessThreshold {
		return purpose + "/large"
	}
	return purpose + "/small"
}
func (h *editProbePayloadHistory) remember(purpose string, payload []byte) {
	if len(payload) > editProbeHistoryLimit || len(purpose) > 256 {
		return
	}
	key := editProbeHistoryKey(purpose, payload)
	old := h.payloads[key]
	if h.bytes-len(old)+len(payload) > editProbeHistoryLimit || old == nil && len(h.payloads) >= editProbeHistoryKeys {
		h.payloads = nil
		h.bytes = 0
		old = nil
	}
	if h.payloads == nil {
		h.payloads = make(map[string][]byte)
	}
	h.payloads[key] = bytes.Clone(payload)
	h.bytes += len(payload) - len(old)
}
func evidencePayload(value any) (string, json.RawMessage, bool) {
	row, ok := value.(map[string]any)
	if !ok {
		return "", nil, false
	}
	payload, ok := row["payload"].(json.RawMessage)
	if !ok {
		return "", nil, false
	}
	purpose, _ := row["purpose"].(string)
	return purpose, payload, true
}
func (h *editProbePayloadHistory) encode(value any) ([]byte, int, error) {
	full, logical, err := encodeEditProbeEvidence(value, true)
	if err != nil || logical > editProbeRawLimit {
		return full, logical, err
	}
	purpose, payload, ok := evidencePayload(value)
	if !ok || len(purpose) > 256 {
		return full, logical, nil
	}
	previous := h.payloads[editProbeHistoryKey(purpose, payload)]
	if len(previous) == 0 {
		return full, logical, nil
	}
	row := value.(map[string]any)
	candidate := make(map[string]any, len(row)+4)
	for k, v := range row {
		candidate[k] = v
	}
	candidate["payload_original_bytes"] = len(payload)
	candidate["payload_sha256"] = fmt.Sprintf("%x", sha256.Sum256(payload))
	candidate["payload_base_sha256"] = fmt.Sprintf("%x", sha256.Sum256(previous))
	if bytes.Equal(payload, previous) {
		candidate["payload_codec"] = editProbeReferenceCodec
		candidate["payload"] = nil
	} else {
		chunks := make([]string, 0, (len(payload)+editProbeChunkBytes-1)/editProbeChunkBytes)
		for offset := 0; offset < len(payload); offset += editProbeChunkBytes {
			end := min(offset+editProbeChunkBytes, len(payload))
			var dict []byte
			if offset < len(previous) {
				dict = previous[offset:min(offset+editProbeChunkBytes, len(previous))]
			}
			var buffer bytes.Buffer
			writer, e := zlib.NewWriterLevelDict(&buffer, zlib.BestSpeed, dict)
			if e != nil {
				return nil, 0, e
			}
			_, e = writer.Write(payload[offset:end])
			closeErr := writer.Close()
			if e != nil || closeErr != nil {
				return nil, 0, errors.Join(e, closeErr)
			}
			chunks = append(chunks, base64.StdEncoding.EncodeToString(buffer.Bytes()))
		}
		candidate["payload_codec"] = editProbeDictionaryCodec
		candidate["payload"] = chunks
	}
	encoded, e := json.Marshal(candidate)
	if e != nil {
		return nil, 0, e
	}
	if len(encoded) < len(full) {
		return encoded, logical, nil
	}
	return full, logical, nil
}
func (h *editProbePayloadHistory) decode(row map[string]json.RawMessage) (json.RawMessage, error) {
	var codec, purpose string
	if raw, present := row["purpose"]; present {
		if err := json.Unmarshal(raw, &purpose); err != nil {
			return nil, err
		}
	}
	if raw, present := row["payload_codec"]; present {
		if err := json.Unmarshal(raw, &codec); err != nil {
			return nil, err
		}
	}
	if codec != editProbeDictionaryCodec && codec != editProbeReferenceCodec {
		payload, err := decodeEditProbeEvidencePayload(row)
		if err == nil {
			h.remember(purpose, payload)
		}
		return payload, err
	}
	if len(purpose) > 256 {
		return nil, errors.New("bounded history purpose required")
	}
	var size *int
	var digest, base string
	if err := json.Unmarshal(row["payload_original_bytes"], &size); err != nil || size == nil || *size < 0 || *size > editProbeRawLimit {
		return nil, errors.New("invalid bounded payload length")
	}
	if err := json.Unmarshal(row["payload_sha256"], &digest); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(row["payload_base_sha256"], &base); err != nil {
		return nil, err
	}
	// The size class is the target's class; every history update uses this rule.
	key := purpose + "/small"
	if *size >= editProbeLosslessThreshold {
		key = purpose + "/large"
	}
	previous := h.payloads[key]
	if len(previous) == 0 || fmt.Sprintf("%x", sha256.Sum256(previous)) != base {
		return nil, errors.New("missing exact prior payload")
	}
	var payload []byte
	if codec == editProbeReferenceCodec {
		if !bytes.Equal(row["payload"], []byte("null")) {
			return nil, errors.New("reference payload must be null")
		}
		payload = bytes.Clone(previous)
	} else {
		var chunks []string
		if len(row["payload"]) > editProbeRawLimit {
			return nil, errors.New("bounded chunk envelope required")
		}
		if err := json.Unmarshal(row["payload"], &chunks); err != nil {
			return nil, err
		}
		if len(chunks) != (*size+editProbeChunkBytes-1)/editProbeChunkBytes {
			return nil, errors.New("wrong exact chunk count")
		}
		payload = make([]byte, 0, *size)
		for i, encoded := range chunks {
			// zlib may expand incompressible input slightly; bound before base64 allocation.
			if len(encoded) > 2*editProbeChunkBytes {
				return nil, errors.New("oversized compressed chunk")
			}
			compressed, err := base64.StdEncoding.Strict().DecodeString(encoded)
			if err != nil {
				return nil, err
			}
			offset := i * editProbeChunkBytes
			var dict []byte
			if offset < len(previous) {
				dict = previous[offset:min(offset+editProbeChunkBytes, len(previous))]
			}
			input := bytes.NewReader(compressed)
			reader, err := zlib.NewReaderDict(input, dict)
			if err != nil {
				return nil, err
			}
			expected := min(editProbeChunkBytes, *size-offset)
			chunk, readErr := io.ReadAll(io.LimitReader(reader, int64(expected)+1))
			closeErr := reader.Close()
			if readErr != nil || closeErr != nil {
				return nil, errors.Join(readErr, closeErr)
			}
			if len(chunk) != expected || input.Len() != 0 {
				return nil, errors.New("incomplete or trailing dictionary chunk")
			}
			payload = append(payload, chunk...)
		}
	}
	if len(payload) != *size || fmt.Sprintf("%x", sha256.Sum256(payload)) != digest {
		return nil, errors.New("incomplete or corrupt original payload")
	}
	h.remember(purpose, payload)
	return json.RawMessage(payload), nil
}

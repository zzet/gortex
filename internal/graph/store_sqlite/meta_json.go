package store_sqlite

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// Node / edge Meta is a map[string]any persisted in the `meta` column.
//
// New rows use a compact, self-describing flat binary codec
// (encodeMetaFast / decodeMetaFast): a 2-byte magic, then a varint entry
// count, then per entry a length-prefixed key, a one-byte type tag, and the
// type's payload. Map keys are sorted before encoding, so the blob is
// deterministic (reproducible for content-hashing / dedup). Each value
// carries its exact Go type tag, so decode reconstructs int / int64 /
// float64 / string / bool / []string / []map[string]any / map[string]any /
// *contracts.Shape exactly — no key-type guessing, and far less CPU and
// allocation than a reflection codec. (The old gob hot path recompiled its
// type engine on every edge, which dominated cold-load CPU and allocation;
// JSON needs no engine but widens every number to float64 and every slice
// to []any, so it needs the metaWire DTO below to recover the exact types.)
//
// A value whose type the flat codec does not model (rare) makes
// encodeMetaFast bail and encodeMeta falls back to JSON for the whole blob;
// decodeMeta still reads such a blob through the metaWire typed DTO. No data
// is ever dropped.
//
// Older on-disk stores hold JSON or gob blobs. decodeMeta sniffs the leading
// bytes to pick the decoder — the flat magic, '{' for JSON, otherwise gob —
// so every prior format still loads and migrates to the flat codec on its
// next write. No schema migration is required: the `meta` column type is
// unchanged.
//
// metaWire (below) remains the decode path for legacy JSON rows and the JSON
// fallback. JSON has one numeric type, so a naive json.Unmarshal into a
// map[string]any widens every number to float64 and every []T to []any,
// silently corrupting readers that type-assert .(int) / .(float64) /
// .([]string) / .(*contracts.Shape); metaWire is a typed DTO whose fields
// parse each known key as its exact Go type and normalises the open tail
// (Extra plus nested maps) with a small key-type table.

// metaWire is the decode-side DTO. Scalar fields are pointers so an absent
// key (nil) is distinguished from a present zero value — comma-ok readers
// rely on that distinction. Slices, maps and Shape are already nil-able.
type metaWire struct {
	// Symbol-shape keys stamped by language extractors (node).
	Signature  *string `json:"signature,omitempty"`
	Visibility *string `json:"visibility,omitempty"`
	Doc        *string `json:"doc,omitempty"`
	External   *bool   `json:"external,omitempty"`

	// Analyzer / contract scalar keys (node).
	Complexity  *int     `json:"complexity,omitempty"`
	LoopDepth   *int     `json:"loop_depth,omitempty"`
	ParseErrors *int     `json:"parse_errors,omitempty"`
	Position    *int     `json:"position,omitempty"`
	Line        *int     `json:"line,omitempty"`
	Confidence  *float64 `json:"confidence,omitempty"`
	CoveragePct *float64 `json:"coverage_pct,omitempty"`

	// Contract structural keys (node).
	Shape            *contracts.Shape `json:"shape,omitempty"`
	ResponseEnvelope []map[string]any `json:"response_envelope,omitempty"`
	PathParamNames   []string         `json:"path_param_names,omitempty"`
	QueryParams      []string         `json:"query_params,omitempty"`
	StatusCodes      []string         `json:"status_codes,omitempty"`

	// Edge scalar keys.
	CandidateCount *int     `json:"candidate_count,omitempty"`
	Similarity     *float64 `json:"similarity,omitempty"`
	Score          *float64 `json:"score,omitempty"`
	Count          *int     `json:"count,omitempty"`
	CloneTokens    *int     `json:"clone_tokens,omitempty"`

	// Nested enrichment maps (sidecar-primary; the meta map is the
	// un-migrated / in-memory fallback). Decoded as plain maps then
	// normalised via the key-type table so their integer children come
	// back as int / int64 rather than float64.
	Churn        map[string]any `json:"churn,omitempty"`
	Coverage     map[string]any `json:"coverage,omitempty"`
	LastAuthored map[string]any `json:"last_authored,omitempty"`
	ContractMeta map[string]any `json:"contract_meta,omitempty"`

	// Extra captures every key not named above (the open / plugin /
	// per-language tail, overwhelmingly strings and bools).
	Extra map[string]any `json:"-"`
}

// metaWireKnownKeys are the JSON keys consumed by metaWire's typed fields;
// everything else is captured into Extra.
var metaWireKnownKeys = []string{
	"signature", "visibility", "doc", "external",
	"complexity", "loop_depth", "parse_errors", "position", "line",
	"confidence", "coverage_pct",
	"shape", "response_envelope", "path_param_names", "query_params", "status_codes",
	"candidate_count", "similarity", "score", "count", "clone_tokens",
	"churn", "coverage", "last_authored", "contract_meta",
}

// metaFloatKeys are keys whose numeric value must stay float64 even when it
// happens to be integral (e.g. confidence 1.0 marshals as "1"); without
// this they would normalise to int and break a .(float64) reader.
var metaFloatKeys = map[string]bool{
	"confidence": true, "coverage_pct": true, "score": true,
	"similarity": true, "churn_rate": true, "rate": true,
}

// metaInt64Keys are keys whose numeric value must be int64 (unix
// timestamps), matching readers that assert .(int64).
var metaInt64Keys = map[string]bool{
	"timestamp": true, "ts": true,
}

// metaStringSliceKeys are keys whose array value must be []string (JSON
// arrays decode to []any); readers assert .([]string).
var metaStringSliceKeys = map[string]bool{
	"path_param_names": true, "query_params": true, "status_codes": true,
	"notes": true, "methods": true, "arg_names": true, "repos": true,
	"usings": true, "global_usings": true, "using_static": true,
	"scoped_usings": true, "global_using_static": true,
}

// metaMapSliceKeys are keys whose array value must be []map[string]any.
var metaMapSliceKeys = map[string]bool{
	"response_envelope": true,
	"ef_fluent":         true,
}

// UnmarshalJSON decodes the typed fields and captures every other key into
// Extra (with UseNumber so the tail keeps int/float fidelity).
func (w *metaWire) UnmarshalJSON(b []byte) error {
	type alias metaWire
	if err := json.Unmarshal(b, (*alias)(w)); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	for _, k := range metaWireKnownKeys {
		delete(raw, k)
	}
	if len(raw) > 0 {
		w.Extra = make(map[string]any, len(raw))
		for k, v := range raw {
			w.Extra[k] = normalizeMetaValue(k, v)
		}
	}
	return nil
}

// toMap rebuilds the in-memory map[string]any with exact Go types.
func (w *metaWire) toMap() map[string]any {
	m := make(map[string]any, len(metaWireKnownKeys)+len(w.Extra))
	putString(m, "signature", w.Signature)
	putString(m, "visibility", w.Visibility)
	putString(m, "doc", w.Doc)
	putBool(m, "external", w.External)
	putInt(m, "complexity", w.Complexity)
	putInt(m, "loop_depth", w.LoopDepth)
	putInt(m, "parse_errors", w.ParseErrors)
	putInt(m, "position", w.Position)
	putInt(m, "line", w.Line)
	putFloat(m, "confidence", w.Confidence)
	putFloat(m, "coverage_pct", w.CoveragePct)
	if w.Shape != nil {
		m["shape"] = w.Shape
	}
	if w.ResponseEnvelope != nil {
		m["response_envelope"] = w.ResponseEnvelope
	}
	if w.PathParamNames != nil {
		m["path_param_names"] = w.PathParamNames
	}
	if w.QueryParams != nil {
		m["query_params"] = w.QueryParams
	}
	if w.StatusCodes != nil {
		m["status_codes"] = w.StatusCodes
	}
	putInt(m, "candidate_count", w.CandidateCount)
	putFloat(m, "similarity", w.Similarity)
	putFloat(m, "score", w.Score)
	putInt(m, "count", w.Count)
	putInt(m, "clone_tokens", w.CloneTokens)
	putNestedMap(m, "churn", w.Churn)
	putNestedMap(m, "coverage", w.Coverage)
	putNestedMap(m, "last_authored", w.LastAuthored)
	putNestedMap(m, "contract_meta", w.ContractMeta)
	for k, v := range w.Extra {
		m[k] = v
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func putString(m map[string]any, k string, v *string) {
	if v != nil {
		m[k] = *v
	}
}

func putBool(m map[string]any, k string, v *bool) {
	if v != nil {
		m[k] = *v
	}
}

func putInt(m map[string]any, k string, v *int) {
	if v != nil {
		m[k] = *v
	}
}

func putFloat(m map[string]any, k string, v *float64) {
	if v != nil {
		m[k] = *v
	}
}

// putNestedMap normalises a nested enrichment map (decoded by the standard
// json path, so its numbers are float64) into exact Go types.
func putNestedMap(m map[string]any, k string, nested map[string]any) {
	if nested == nil {
		return
	}
	out := make(map[string]any, len(nested))
	for nk, nv := range nested {
		out[nk] = normalizeMetaValue(nk, nv)
	}
	m[k] = out
}

// normalizeMetaValue coerces a json-decoded value to the exact Go type the
// readers expect, recursing through nested maps and slices. It accepts both
// json.Number (the Extra path uses UseNumber) and float64 (the typed-field
// path decodes nested maps with standard json), so it is correct for both.
func normalizeMetaValue(key string, v any) any {
	switch vv := v.(type) {
	case json.Number:
		return normalizeNumber(key, numberToFloat(vv), &vv)
	case float64:
		return normalizeNumber(key, vv, nil)
	case []any:
		return normalizeSlice(key, vv)
	case map[string]any:
		out := make(map[string]any, len(vv))
		for nk, nv := range vv {
			out[nk] = normalizeMetaValue(nk, nv)
		}
		return out
	default:
		return v
	}
}

func numberToFloat(n json.Number) float64 {
	f, _ := n.Float64()
	return f
}

// normalizeNumber picks the Go numeric type for key. num is the float view;
// jn (may be nil) is the original json.Number for exact integer recovery.
func normalizeNumber(key string, num float64, jn *json.Number) any {
	if metaFloatKeys[key] {
		return num
	}
	if metaInt64Keys[key] {
		if jn != nil {
			if i, err := jn.Int64(); err == nil {
				return i
			}
		}
		return int64(num)
	}
	if num == float64(int64(num)) {
		if jn != nil {
			if i, err := jn.Int64(); err == nil {
				return int(i)
			}
		}
		return int(num)
	}
	return num
}

func normalizeSlice(key string, s []any) any {
	if metaStringSliceKeys[key] {
		out := make([]string, 0, len(s))
		for _, e := range s {
			if str, ok := e.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	if metaMapSliceKeys[key] {
		out := make([]map[string]any, 0, len(s))
		for _, e := range s {
			if mm, ok := e.(map[string]any); ok {
				norm := make(map[string]any, len(mm))
				for nk, nv := range mm {
					norm[nk] = normalizeMetaValue(nk, nv)
				}
				out = append(out, norm)
			}
		}
		return out
	}
	out := make([]any, len(s))
	for i, e := range s {
		out[i] = normalizeMetaValue(key, e)
	}
	return out
}

// encodeMeta serialises Meta. nil / empty Meta stores as NULL. The common
// case is the flat binary codec; a value type the flat codec doesn't model
// falls back to JSON for the whole blob (decodeMeta auto-detects it by the
// leading '{').
func encodeMeta(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	if b, ok := encodeMetaFast(m); ok {
		return b, nil
	}
	return json.Marshal(m)
}

// decodeMeta reads a meta blob, picking the decoder from the leading bytes:
// the flat magic => the flat binary codec; '{' => JSON (routed through
// metaWire for exact types); otherwise legacy gob.
func decodeMeta(b []byte) (map[string]any, error) {
	if len(b) == 0 {
		return nil, nil
	}
	if isFlatMeta(b) {
		return decodeMetaFast(b)
	}
	if isJSONObject(b) {
		var w metaWire
		if err := json.Unmarshal(b, &w); err != nil {
			// A gob blob whose first byte is '{' would land here; fall
			// back rather than fail the row.
			return decodeMetaGob(b)
		}
		return w.toMap(), nil
	}
	return decodeMetaGob(b)
}

// isJSONObject reports whether b looks like a JSON object (the only shape
// encodeMeta ever produces). Leading whitespace is tolerated.
func isJSONObject(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}

func decodeMetaGob(b []byte) (map[string]any, error) {
	var m map[string]any
	if err := gob.NewDecoder(bytes.NewReader(b)).Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// -- flat binary meta codec -----------------------------------------------
//
// Layout: metaFlatMagic0, metaFlatVersion, then a map body. A map body is a
// uvarint entry count followed by that many [uvarint keyLen][key bytes]
// [1-byte type tag][value] entries, keys sorted. Value layouts are listed on
// the tag constants below. The format is self-describing (every value
// carries its type), so decode is exact with no key-type heuristics.
//
// metaFlatMagic0 is 0x00: a non-empty gob stream never begins with 0x00 (its
// leading message-length prefix is always > 0), and a JSON object always
// begins with '{' (0x7B) — so a leading 0x00 unambiguously marks the flat
// format and never collides with a legacy blob. metaFlatVersion guards the
// layout if it ever changes.
const (
	metaFlatMagic0  = 0x00
	metaFlatVersion = 0x01
)

// Flat value type tags. The byte after a key selects the value layout.
const (
	metaTagNil      = 0x01 // (no payload)
	metaTagString   = 0x02 // uvarint len, len bytes
	metaTagBool     = 0x03 // 1 byte (0 / 1)
	metaTagInt      = 0x04 // zig-zag varint
	metaTagInt64    = 0x05 // zig-zag varint
	metaTagFloat64  = 0x06 // 8 bytes little-endian IEEE-754 bits
	metaTagStrSlice = 0x07 // uvarint count, then count strings
	metaTagMap      = 0x08 // a map body (recursive)
	metaTagMapSlice = 0x09 // uvarint count, then count map bodies
	metaTagAnySlice = 0x0A // uvarint count, then count tagged values
	metaTagShape    = 0x0B // uvarint len, len bytes of JSON-encoded *contracts.Shape
	metaTagUint64   = 0x0C // unsigned varint
	metaTagF64Slice = 0x0D // uvarint count, then count little-endian float64 values
)

var errMetaTruncated = errors.New("store_sqlite: truncated meta blob")

// isFlatMeta reports whether b is a flat-codec blob.
func isFlatMeta(b []byte) bool {
	return len(b) >= 2 && b[0] == metaFlatMagic0 && b[1] == metaFlatVersion
}

// encodeMetaFast serialises m with the flat binary codec. It returns ok=false
// (and the caller falls back to JSON) when any value — at any depth — has a
// type the codec does not model, so no data is ever silently dropped.
func encodeMetaFast(m map[string]any) (b []byte, ok bool) {
	buf := make([]byte, 0, 32+len(m)*24)
	buf = append(buf, metaFlatMagic0, metaFlatVersion)
	buf, ok = appendMetaMap(buf, m)
	if !ok {
		return nil, false
	}
	return buf, true
}

// appendMetaMap writes a map body (count + sorted entries). Sorting the keys
// makes the encoding deterministic.
func appendMetaMap(buf []byte, m map[string]any) ([]byte, bool) {
	buf = binary.AppendUvarint(buf, uint64(len(m)))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		buf = appendMetaString(buf, k)
		var ok bool
		buf, ok = appendMetaValue(buf, m[k])
		if !ok {
			return buf, false
		}
	}
	return buf, true
}

func appendMetaString(buf []byte, s string) []byte {
	buf = binary.AppendUvarint(buf, uint64(len(s)))
	return append(buf, s...)
}

// appendMetaValue writes a tagged value, returning ok=false for an
// unmodelled type.
func appendMetaValue(buf []byte, v any) ([]byte, bool) {
	switch vv := v.(type) {
	case nil:
		return append(buf, metaTagNil), true
	case string:
		buf = append(buf, metaTagString)
		return appendMetaString(buf, vv), true
	case bool:
		bb := byte(0)
		if vv {
			bb = 1
		}
		return append(buf, metaTagBool, bb), true
	case int:
		buf = append(buf, metaTagInt)
		return binary.AppendVarint(buf, int64(vv)), true
	case int64:
		buf = append(buf, metaTagInt64)
		return binary.AppendVarint(buf, vv), true
	case uint64:
		buf = append(buf, metaTagUint64)
		return binary.AppendUvarint(buf, vv), true
	case float64:
		buf = append(buf, metaTagFloat64)
		return binary.LittleEndian.AppendUint64(buf, math.Float64bits(vv)), true
	case []float64:
		buf = append(buf, metaTagF64Slice)
		buf = binary.AppendUvarint(buf, uint64(len(vv)))
		for _, f := range vv {
			buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(f))
		}
		return buf, true
	case []string:
		buf = append(buf, metaTagStrSlice)
		buf = binary.AppendUvarint(buf, uint64(len(vv)))
		for _, s := range vv {
			buf = appendMetaString(buf, s)
		}
		return buf, true
	case map[string]any:
		buf = append(buf, metaTagMap)
		return appendMetaMap(buf, vv)
	case []map[string]any:
		buf = append(buf, metaTagMapSlice)
		buf = binary.AppendUvarint(buf, uint64(len(vv)))
		for _, mm := range vv {
			var ok bool
			buf, ok = appendMetaMap(buf, mm)
			if !ok {
				return buf, false
			}
		}
		return buf, true
	case []any:
		buf = append(buf, metaTagAnySlice)
		buf = binary.AppendUvarint(buf, uint64(len(vv)))
		for _, e := range vv {
			var ok bool
			buf, ok = appendMetaValue(buf, e)
			if !ok {
				return buf, false
			}
		}
		return buf, true
	case *contracts.Shape:
		js, err := json.Marshal(vv)
		if err != nil {
			return buf, false
		}
		buf = append(buf, metaTagShape)
		return appendMetaString(buf, string(js)), true
	default:
		return buf, false
	}
}

// decodeMetaFast reverses encodeMetaFast. A malformed blob returns an error
// (never panics).
func decodeMetaFast(b []byte) (map[string]any, error) {
	if len(b) < 2 {
		return nil, errMetaTruncated
	}
	d := &metaDecoder{buf: b[2:]} // skip magic + version
	m, err := d.readMap()
	if err != nil {
		return nil, err
	}
	return m, nil
}

// metaDecoder is a bounds-checked cursor over a flat meta blob.
type metaDecoder struct {
	buf []byte
	pos int
}

func (d *metaDecoder) uvarint() (uint64, error) {
	v, n := binary.Uvarint(d.buf[d.pos:])
	if n <= 0 {
		return 0, errMetaTruncated
	}
	d.pos += n
	return v, nil
}

func (d *metaDecoder) varint() (int64, error) {
	v, n := binary.Varint(d.buf[d.pos:])
	if n <= 0 {
		return 0, errMetaTruncated
	}
	d.pos += n
	return v, nil
}

func (d *metaDecoder) readByte() (byte, error) {
	if d.pos >= len(d.buf) {
		return 0, errMetaTruncated
	}
	b := d.buf[d.pos]
	d.pos++
	return b, nil
}

func (d *metaDecoder) readBytes(n int) ([]byte, error) {
	if n < 0 || n > len(d.buf)-d.pos {
		return nil, errMetaTruncated
	}
	b := d.buf[d.pos : d.pos+n]
	d.pos += n
	return b, nil
}

func (d *metaDecoder) readString() (string, error) {
	n, err := d.uvarint()
	if err != nil {
		return "", err
	}
	if n > uint64(len(d.buf)-d.pos) {
		return "", errMetaTruncated
	}
	b, err := d.readBytes(int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// readCount reads a uvarint length and caps it to the remaining buffer so a
// corrupt count cannot trigger a giant allocation (every element occupies at
// least one byte).
func (d *metaDecoder) readCount() (int, error) {
	n, err := d.uvarint()
	if err != nil {
		return 0, err
	}
	if n > uint64(len(d.buf)-d.pos) {
		return 0, errMetaTruncated
	}
	return int(n), nil
}

func (d *metaDecoder) readMap() (map[string]any, error) {
	count, err := d.readCount()
	if err != nil {
		return nil, err
	}
	m := make(map[string]any, count)
	for i := 0; i < count; i++ {
		key, err := d.readString()
		if err != nil {
			return nil, err
		}
		val, err := d.readValue()
		if err != nil {
			return nil, err
		}
		m[key] = val
	}
	return m, nil
}

func (d *metaDecoder) readValue() (any, error) {
	tag, err := d.readByte()
	if err != nil {
		return nil, err
	}
	switch tag {
	case metaTagNil:
		return nil, nil
	case metaTagString:
		return d.readString()
	case metaTagBool:
		b, err := d.readByte()
		if err != nil {
			return nil, err
		}
		return b != 0, nil
	case metaTagInt:
		v, err := d.varint()
		if err != nil {
			return nil, err
		}
		return int(v), nil
	case metaTagInt64:
		v, err := d.varint()
		if err != nil {
			return nil, err
		}
		return v, nil
	case metaTagUint64:
		return d.uvarint()
	case metaTagFloat64:
		b, err := d.readBytes(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(b)), nil
	case metaTagF64Slice:
		count, err := d.readCount()
		if err != nil {
			return nil, err
		}
		out := make([]float64, count)
		for i := range out {
			b, err := d.readBytes(8)
			if err != nil {
				return nil, err
			}
			out[i] = math.Float64frombits(binary.LittleEndian.Uint64(b))
		}
		return out, nil
	case metaTagStrSlice:
		count, err := d.readCount()
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, count)
		for i := 0; i < count; i++ {
			s, err := d.readString()
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, nil
	case metaTagMap:
		return d.readMap()
	case metaTagMapSlice:
		count, err := d.readCount()
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, count)
		for i := 0; i < count; i++ {
			mm, err := d.readMap()
			if err != nil {
				return nil, err
			}
			out = append(out, mm)
		}
		return out, nil
	case metaTagAnySlice:
		count, err := d.readCount()
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, count)
		for i := 0; i < count; i++ {
			e, err := d.readValue()
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, nil
	case metaTagShape:
		s, err := d.readString()
		if err != nil {
			return nil, err
		}
		var sh *contracts.Shape
		if err := json.Unmarshal([]byte(s), &sh); err != nil {
			return nil, err
		}
		return sh, nil
	default:
		return nil, fmt.Errorf("store_sqlite: unknown meta value tag 0x%02x", tag)
	}
}

// -- promoted node columns ------------------------------------------------
//
// signature / visibility / doc / external are universal, hot-read node
// keys. They are lifted into dedicated nullable columns: stripped from the
// JSON blob on write (extractPromotedMeta) and restored into Meta on read
// (restorePromotedMeta), so the in-memory map is unchanged while the keys
// become queryable and the common blob shrinks.

var promotedMetaColumns = []struct {
	name string
	ddl  string
}{
	{"signature", "signature TEXT"},
	{"visibility", "visibility TEXT"},
	{"doc", "doc TEXT"},
	{"external", "external INTEGER"},
	{"return_type", "return_type TEXT"},
	{"is_async", "is_async INTEGER"},
	{"is_static", "is_static INTEGER"},
	{"is_abstract", "is_abstract INTEGER"},
	{"is_exported", "is_exported INTEGER"},
	{"updated_at", "updated_at INTEGER"},
	{"data_class", "data_class TEXT"},
	{"semantic_type", "semantic_type TEXT"},
	{"semantic_source", "semantic_source TEXT"},
	{"clone_sig", "clone_sig TEXT"},
	{"entry_point", "entry_point INTEGER"},
	{"entry_point_kind", "entry_point_kind TEXT"},
	{"search_signature", "search_signature TEXT"},
	{"search_qual_name", "search_qual_name TEXT"},
	{"search_doc", "search_doc TEXT"},
	{"search_metadata_suppressed", "search_metadata_suppressed INTEGER"},
	{"section_text", "section_text TEXT"},
}

// structNodeColumns are typed nodes columns read and written directly from
// Node struct fields (not the Meta blob): the source column offsets. They are
// NOT NULL DEFAULT 0 like start_line / end_line, so an ALTER on an existing DB
// backfills 0.
var structNodeColumns = []struct {
	name string
	ddl  string
}{
	{"start_column", "start_column INTEGER NOT NULL DEFAULT 0"},
	{"end_column", "end_column INTEGER NOT NULL DEFAULT 0"},
}

// promotedNodeMeta holds the typed column values lifted out of a node's Meta
// blob. A NULL (invalid) field means the key was absent (or had an unexpected
// type and stayed in the blob).
type promotedNodeMeta struct {
	sig, vis, doc, returnType, dataClass                sql.NullString
	semanticType, semanticSource, cloneSig              sql.NullString
	entryPointKind                                      sql.NullString
	searchSig, searchQualName, searchDoc, sectionText   sql.NullString
	external, isAsync, isStatic, isAbstract, isExported sql.NullBool
	entryPoint, searchSuppressed                        sql.NullBool
	updatedAt                                           sql.NullInt64
}

// ensureNodeColumns adds the promoted + struct columns to a nodes table
// created before they existed. A fresh DB already has them from the DDL, so
// this is a no-op; an older DB is altered in place.
func ensureNodeColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(nodes)`)
	if err != nil {
		return err
	}
	existing := make(map[string]bool)
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	add := func(name, ddl string) error {
		if existing[name] {
			return nil
		}
		_, err := db.Exec(`ALTER TABLE nodes ADD COLUMN ` + ddl)
		return err
	}
	for _, c := range structNodeColumns {
		if err := add(c.name, c.ddl); err != nil {
			return err
		}
	}
	for _, c := range promotedMetaColumns {
		if err := add(c.name, c.ddl); err != nil {
			return err
		}
	}
	return nil
}

// extractPromotedMeta splits the promoted keys out of m into typed column
// values and returns the remaining map destined for the JSON blob. m is
// not mutated; a copy is made only when a promoted key is present and has
// the expected type (otherwise the value stays in the blob).
func extractPromotedMeta(m map[string]any) (p promotedNodeMeta, rest map[string]any) {
	rest = m
	if len(m) == 0 {
		return
	}
	has := false
	for _, c := range promotedMetaColumns {
		if _, ok := m[c.name]; ok {
			has = true
			break
		}
	}
	if !has {
		return
	}
	rest = make(map[string]any, len(m))
	str := func(v any) (sql.NullString, bool) {
		if s, ok := v.(string); ok {
			return sql.NullString{String: s, Valid: true}, true
		}
		return sql.NullString{}, false
	}
	boolean := func(v any) (sql.NullBool, bool) {
		if b, ok := v.(bool); ok {
			return sql.NullBool{Bool: b, Valid: true}, true
		}
		return sql.NullBool{}, false
	}
	for k, v := range m {
		var promoted bool
		switch k {
		case "signature":
			if nv, ok := str(v); ok {
				p.sig, promoted = nv, true
			}
		case "visibility":
			if nv, ok := str(v); ok {
				p.vis, promoted = nv, true
			}
		case "doc":
			if nv, ok := str(v); ok {
				p.doc, promoted = nv, true
			}
		case "return_type":
			if nv, ok := str(v); ok {
				p.returnType, promoted = nv, true
			}
		case "data_class":
			if nv, ok := str(v); ok {
				p.dataClass, promoted = nv, true
			}
		case "semantic_type":
			if nv, ok := str(v); ok {
				p.semanticType, promoted = nv, true
			}
		case "semantic_source":
			if nv, ok := str(v); ok {
				p.semanticSource, promoted = nv, true
			}
		case "clone_sig":
			if nv, ok := str(v); ok {
				p.cloneSig, promoted = nv, true
			}
		case "entry_point":
			if nv, ok := boolean(v); ok {
				p.entryPoint, promoted = nv, true
			}
		case "entry_point_kind":
			if nv, ok := str(v); ok {
				p.entryPointKind, promoted = nv, true
			}
		case "search_signature":
			if nv, ok := str(v); ok {
				p.searchSig, promoted = nv, true
			}
		case "search_qual_name":
			if nv, ok := str(v); ok {
				p.searchQualName, promoted = nv, true
			}
		case "search_doc":
			if nv, ok := str(v); ok {
				p.searchDoc, promoted = nv, true
			}
		case "search_metadata_suppressed":
			if nv, ok := boolean(v); ok {
				p.searchSuppressed, promoted = nv, true
			}
		case "section_text":
			if nv, ok := str(v); ok {
				p.sectionText, promoted = nv, true
			}
		case "external":
			if nv, ok := boolean(v); ok {
				p.external, promoted = nv, true
			}
		case "is_async":
			if nv, ok := boolean(v); ok {
				p.isAsync, promoted = nv, true
			}
		case "is_static":
			if nv, ok := boolean(v); ok {
				p.isStatic, promoted = nv, true
			}
		case "is_abstract":
			if nv, ok := boolean(v); ok {
				p.isAbstract, promoted = nv, true
			}
		case "is_exported":
			if nv, ok := boolean(v); ok {
				p.isExported, promoted = nv, true
			}
		case "updated_at":
			if i, ok := metaToInt64(v); ok {
				p.updatedAt, promoted = sql.NullInt64{Int64: i, Valid: true}, true
			}
		}
		if !promoted {
			// Absent / wrong type: keep the value in the JSON blob.
			rest[k] = v
		}
	}
	return
}

// metaToInt64 coerces a meta numeric value (int / int64 / float64 / json.Number)
// to int64 for a promoted timestamp column.
func metaToInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		return int64(x), true
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}

// restorePromotedMeta writes the non-NULL promoted columns back into the
// node's Meta. A NULL column is left alone so a legacy gob row's blob value
// survives.
func restorePromotedMeta(n *graph.Node, p promotedNodeMeta) {
	if !p.sig.Valid && !p.vis.Valid && !p.doc.Valid && !p.returnType.Valid &&
		!p.dataClass.Valid && !p.semanticType.Valid && !p.semanticSource.Valid &&
		!p.cloneSig.Valid && !p.external.Valid && !p.isAsync.Valid &&
		!p.isStatic.Valid && !p.isAbstract.Valid && !p.isExported.Valid &&
		!p.updatedAt.Valid && !p.entryPoint.Valid && !p.entryPointKind.Valid &&
		!p.searchSig.Valid && !p.searchQualName.Valid && !p.searchDoc.Valid &&
		!p.searchSuppressed.Valid && !p.sectionText.Valid {
		return
	}
	if n.Meta == nil {
		n.Meta = make(map[string]any, 8)
	}
	if p.sig.Valid {
		n.Meta["signature"] = p.sig.String
	}
	if p.vis.Valid {
		n.Meta["visibility"] = p.vis.String
	}
	if p.doc.Valid {
		n.Meta["doc"] = p.doc.String
	}
	if p.returnType.Valid {
		n.Meta["return_type"] = p.returnType.String
	}
	if p.dataClass.Valid {
		n.Meta["data_class"] = p.dataClass.String
	}
	if p.semanticType.Valid {
		n.Meta["semantic_type"] = p.semanticType.String
	}
	if p.semanticSource.Valid {
		n.Meta["semantic_source"] = p.semanticSource.String
	}
	if p.cloneSig.Valid {
		n.Meta["clone_sig"] = p.cloneSig.String
	}
	if p.entryPoint.Valid {
		n.Meta["entry_point"] = p.entryPoint.Bool
	}
	if p.entryPointKind.Valid {
		n.Meta["entry_point_kind"] = p.entryPointKind.String
	}
	if p.searchSig.Valid {
		n.Meta["search_signature"] = p.searchSig.String
	}
	if p.searchQualName.Valid {
		n.Meta["search_qual_name"] = p.searchQualName.String
	}
	if p.searchDoc.Valid {
		n.Meta["search_doc"] = p.searchDoc.String
	}
	if p.searchSuppressed.Valid {
		n.Meta["search_metadata_suppressed"] = p.searchSuppressed.Bool
	}
	if p.sectionText.Valid {
		n.Meta["section_text"] = p.sectionText.String
	}
	if p.external.Valid {
		n.Meta["external"] = p.external.Bool
	}
	if p.isAsync.Valid {
		n.Meta["is_async"] = p.isAsync.Bool
	}
	if p.isStatic.Valid {
		n.Meta["is_static"] = p.isStatic.Bool
	}
	if p.isAbstract.Valid {
		n.Meta["is_abstract"] = p.isAbstract.Bool
	}
	if p.isExported.Valid {
		n.Meta["is_exported"] = p.isExported.Bool
	}
	if p.updatedAt.Valid {
		n.Meta["updated_at"] = p.updatedAt.Int64
	}
}

// -- promoted edge columns -------------------------------------------------
//
// resolve_terminal / resolve_terminal_reason (see resolver/terminal.go) and
// semantic_source are the edge-side sibling of promotedMetaColumns above. A generated column
// deriving them from the meta blob via json_extract was tried first and
// abandoned: encodeMeta's common case is a custom flat binary codec (see
// "flat binary meta codec" below in this file), not JSON — JSON is only a
// fallback for value shapes the flat codec can't model, and a bool/string
// pair like these two always fits the flat codec. json_extract/json_valid
// against a real store's meta blobs therefore either throws ("malformed
// JSON") or, once gated by json_valid, silently evaluates to NULL for
// effectively every row. Promoting the keys out of the blob into typed
// columns (exactly the node-side pattern) sidesteps the encoding entirely:
// extractPromotedEdgeMeta/restorePromotedEdgeMeta operate on the already-
// decoded map[string]any, not the raw bytes, so they work identically
// regardless of which codec wrote the blob.

// promotedEdgeMeta holds the typed column values lifted out of an edge's
// Meta blob. A NULL (invalid) field means the key was absent (or had an
// unexpected type and stayed in the blob).
type promotedEdgeMeta struct {
	resolveTerminal       sql.NullBool
	resolveTerminalReason sql.NullString
	semanticSource        sql.NullString
}

// extractPromotedEdgeMeta splits resolve_terminal / resolve_terminal_reason /
// semantic_source out of m into typed column values and returns the remaining map destined
// for the meta blob. m is not mutated; a copy is made only when one of the
// promoted keys is present with the expected type.
func extractPromotedEdgeMeta(m map[string]any) (p promotedEdgeMeta, rest map[string]any) {
	rest = m
	if len(m) == 0 {
		return
	}
	_, hasTerminal := m["resolve_terminal"]
	_, hasReason := m["resolve_terminal_reason"]
	_, hasSemanticSource := m["semantic_source"]
	if !hasTerminal && !hasReason && !hasSemanticSource {
		return
	}
	rest = make(map[string]any, len(m))
	for k, v := range m {
		var promoted bool
		switch k {
		case "resolve_terminal":
			if b, ok := v.(bool); ok {
				p.resolveTerminal, promoted = sql.NullBool{Bool: b, Valid: true}, true
			}
		case "resolve_terminal_reason":
			if s, ok := v.(string); ok {
				p.resolveTerminalReason, promoted = sql.NullString{String: s, Valid: true}, true
			}
		case "semantic_source":
			if s, ok := v.(string); ok {
				p.semanticSource, promoted = sql.NullString{String: s, Valid: true}, true
			}
		}
		if !promoted {
			rest[k] = v
		}
	}
	return
}

// restorePromotedEdgeMeta writes the non-NULL promoted columns back into
// the edge's Meta. A NULL column is left alone so a pre-promotion row's
// blob-carried value (if any) survives.
func restorePromotedEdgeMeta(e *graph.Edge, p promotedEdgeMeta) {
	if !p.resolveTerminal.Valid && !p.resolveTerminalReason.Valid && !p.semanticSource.Valid {
		return
	}
	if e.Meta == nil {
		e.Meta = make(map[string]any, 3)
	}
	if p.resolveTerminal.Valid {
		e.Meta["resolve_terminal"] = p.resolveTerminal.Bool
	}
	if p.resolveTerminalReason.Valid {
		e.Meta["resolve_terminal_reason"] = p.resolveTerminalReason.String
	}
	if p.semanticSource.Valid {
		e.Meta["semantic_source"] = p.semanticSource.String
	}
}

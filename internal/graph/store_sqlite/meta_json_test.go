package store_sqlite

import (
	"bytes"
	"encoding/gob"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/contracts"
)

// roundTrip encodes Meta with the flat codec and decodes it back, the
// persist->reload path every reader sees after a daemon restart / store
// hydration.
func roundTrip(t *testing.T, in map[string]any) map[string]any {
	t.Helper()
	b, err := encodeMeta(in)
	if err != nil {
		t.Fatalf("encodeMeta: %v", err)
	}
	if !isFlatMeta(b) {
		t.Fatalf("encodeMeta did not produce a flat-codec blob: %q", b)
	}
	out, err := decodeMeta(b)
	if err != nil {
		t.Fatalf("decodeMeta: %v", err)
	}
	return out
}

// TestMetaRoundTripExactTypes is the fidelity canary: every key the audit
// found read with a raw type-assertion must survive a JSON round-trip with
// its exact Go type, or the corresponding reader silently breaks.
func TestDecodeLegacyJSONEFFluentExactType(t *testing.T) {
	got, err := decodeMeta([]byte(`{
		"ef_fluent": [{
			"context": "ProbeContext",
			"kind": "mapping",
			"line": 12,
			"ordinal": 0,
			"entity": "Widget",
			"table": "widget|rows",
			"schema": "core",
			"relation": "table"
		}]
	}`))
	if err != nil {
		t.Fatalf("decode legacy JSON ef_fluent: %v", err)
	}
	actions, ok := got["ef_fluent"].([]map[string]any)
	if !ok {
		t.Fatalf("ef_fluent type = %T, want []map[string]any", got["ef_fluent"])
	}
	if len(actions) != 1 {
		t.Fatalf("ef_fluent length = %d, want 1", len(actions))
	}
	assertType[string](t, actions[0], "table", "widget|rows")
	assertType[int](t, actions[0], "line", 12)
	assertType[int](t, actions[0], "ordinal", 0)
}

func TestMetaRoundTripExactTypes(t *testing.T) {
	var efWire metaWire
	if err := efWire.UnmarshalJSON([]byte(`{"ef_fluent":[{"context":"ProbeContext","kind":"mapping","line":11,"ordinal":0,"entity":"Widget","table":"widgets","schema":"","relation":"table"}]}`)); err != nil {
		t.Fatalf("decode structured ef_fluent: %v", err)
	}
	actions, ok := efWire.Extra["ef_fluent"].([]map[string]any)
	if !ok {
		t.Fatalf("structured ef_fluent type = %T, want []map[string]any", efWire.Extra["ef_fluent"])
	}
	if len(actions) != 1 || actions[0]["kind"] != "mapping" || actions[0]["entity"] != "Widget" {
		t.Fatalf("structured ef_fluent = %#v", actions)
	}

	shape := &contracts.Shape{
		Kind:   "struct",
		Fields: []contracts.ShapeField{{Name: "id", Type: "int64", Required: true}},
		Notes:  []string{"partial"},
	}
	node := map[string]any{
		"signature":         "func F(x int) error",
		"visibility":        "public",
		"doc":               "F does a thing.",
		"external":          true,
		"complexity":        7,
		"loop_depth":        2,
		"parse_errors":      0,
		"position":          3,
		"line":              42,
		"confidence":        1.0, // integral float — must stay float64
		"coverage_pct":      83.5,
		"shape":             shape,
		"response_envelope": []map[string]any{{"name": "data", "type": "User"}},
		"path_param_names":  []string{"id", "org"},
		"query_params":      []string{"limit"},
		"status_codes":      []string{"200", "404"},
		"churn":             map[string]any{"commit_count": 12, "age_days": 365, "churn_rate": 2.0, "last_author": "a@b.c"},
		"coverage":          map[string]any{"num_stmt": 40, "hit": 33},
		"last_authored":     map[string]any{"timestamp": int64(1700000000), "email": "x@y.z"},
		"some_plugin_flag":  "go_linkname", // Extra tail (string)
		"is_generated":      false,         // Extra tail (bool)
	}
	got := roundTrip(t, node)

	assertType[int](t, got, "complexity", 7)
	assertType[int](t, got, "loop_depth", 2)
	assertType[int](t, got, "parse_errors", 0)
	assertType[int](t, got, "position", 3)
	assertType[int](t, got, "line", 42)
	assertType[float64](t, got, "confidence", 1.0)
	assertType[float64](t, got, "coverage_pct", 83.5)
	assertType[string](t, got, "signature", "func F(x int) error")
	assertType[string](t, got, "visibility", "public")
	assertType[bool](t, got, "external", true)
	assertType[string](t, got, "some_plugin_flag", "go_linkname")
	assertType[bool](t, got, "is_generated", false)

	// Shape must rebuild as *contracts.Shape, not map[string]any.
	gotShape, ok := got["shape"].(*contracts.Shape)
	if !ok {
		t.Fatalf("shape: want *contracts.Shape, got %T", got["shape"])
	}
	if !reflect.DeepEqual(gotShape, shape) {
		t.Errorf("shape mismatch: %+v vs %+v", gotShape, shape)
	}

	// response_envelope must be []map[string]any, not []any.
	if _, ok := got["response_envelope"].([]map[string]any); !ok {
		t.Errorf("response_envelope: want []map[string]any, got %T", got["response_envelope"])
	}
	// []string keys.
	for _, k := range []string{"path_param_names", "query_params", "status_codes"} {
		if _, ok := got[k].([]string); !ok {
			t.Errorf("%s: want []string, got %T", k, got[k])
		}
	}

	// Nested map children keep exact types.
	churn := got["churn"].(map[string]any)
	assertType[int](t, churn, "commit_count", 12)
	assertType[int](t, churn, "age_days", 365)
	assertType[float64](t, churn, "churn_rate", 2.0) // integral float, nested
	assertType[string](t, churn, "last_author", "a@b.c")
	cov := got["coverage"].(map[string]any)
	assertType[int](t, cov, "num_stmt", 40)
	assertType[int](t, cov, "hit", 33)
	la := got["last_authored"].(map[string]any)
	assertType[int64](t, la, "timestamp", int64(1700000000))
}

func TestEdgeMetaRoundTripExactTypes(t *testing.T) {
	edge := map[string]any{
		"candidate_count": 2,
		"similarity":      0.875,
		"score":           1.0, // integral float — must stay float64
		"count":           5,
		"clone_tokens":    128,
		"synthesized_by":  "grpc", // Extra tail
	}
	got := roundTrip(t, edge)
	assertType[int](t, got, "candidate_count", 2)
	assertType[float64](t, got, "similarity", 0.875)
	assertType[float64](t, got, "score", 1.0)
	assertType[int](t, got, "count", 5)
	assertType[int](t, got, "clone_tokens", 128)
	assertType[string](t, got, "synthesized_by", "grpc")
}

// TestDecodeLegacyGob proves existing on-disk gob blobs still decode.
func TestDecodeLegacyGob(t *testing.T) {
	orig := map[string]any{"visibility": "private", "complexity": 9, "confidence": 1.0}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(orig); err != nil {
		t.Fatalf("gob encode: %v", err)
	}
	got, err := decodeMeta(buf.Bytes())
	if err != nil {
		t.Fatalf("decodeMeta(gob): %v", err)
	}
	// gob preserves exact types natively.
	assertType[string](t, got, "visibility", "private")
	assertType[int](t, got, "complexity", 9)
	assertType[float64](t, got, "confidence", 1.0)
}

func TestEncodeMetaEmpty(t *testing.T) {
	b, err := encodeMeta(nil)
	if err != nil || b != nil {
		t.Fatalf("encodeMeta(nil) = %q, %v; want nil, nil", b, err)
	}
	b, err = encodeMeta(map[string]any{})
	if err != nil || b != nil {
		t.Fatalf("encodeMeta(empty) = %q, %v; want nil, nil", b, err)
	}
	m, err := decodeMeta(nil)
	if err != nil || m != nil {
		t.Fatalf("decodeMeta(nil) = %v, %v; want nil, nil", m, err)
	}
}

func TestMetaRoundTripReachExactTypes(t *testing.T) {
	wantConf := []float64{0, 0.625, 1}
	got := roundTrip(t, map[string]any{
		"reach_build":   uint64(1<<63 + 17),
		"reach_d1_conf": wantConf,
	})
	if build, ok := got["reach_build"].(uint64); !ok || build != uint64(1<<63+17) {
		t.Fatalf("reach_build type/value = %T(%v), want uint64", got["reach_build"], got["reach_build"])
	}
	if conf, ok := got["reach_d1_conf"].([]float64); !ok || !reflect.DeepEqual(conf, wantConf) {
		t.Fatalf("reach_d1_conf type/value = %T(%v), want []float64(%v)", got["reach_d1_conf"], got["reach_d1_conf"], wantConf)
	}
}

func assertType[T comparable](t *testing.T, m map[string]any, key string, want T) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Errorf("%s: missing from decoded map", key)
		return
	}
	got, ok := v.(T)
	if !ok {
		t.Errorf("%s: want type %T, got %T (value %v)", key, want, v, v)
		return
	}
	if got != want {
		t.Errorf("%s: want %v, got %v", key, want, got)
	}
}

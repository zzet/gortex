package contracts

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContractRecordFingerprintsSeparateLineAndKeepSemanticInputs(t *testing.T) {
	base := Contract{ID: "http::GET /items", Type: ContractHTTP, Role: RoleProvider,
		SymbolID: "repo/handler", FilePath: "repo/routes.go", Line: 3,
		RepoPrefix: "repo", WorkspaceID: "workspace", ProjectID: "project", Confidence: 0.9,
		Meta: map[string]any{"response_fields": map[string]any{"count": "int"}}}
	initial, err := FingerprintRecords([]Contract{base})
	require.NoError(t, err)
	moved := base
	moved.Line++
	location, err := FingerprintRecords([]Contract{moved})
	require.NoError(t, err)
	require.Equal(t, initial.Semantic, location.Semantic)
	require.NotEqual(t, initial.Full, location.Full)
	for _, mutate := range []func(*Contract){
		func(c *Contract) { c.Meta = map[string]any{"response_fields": map[string]any{"count": "string"}} },
		func(c *Contract) { c.Confidence = 0.5 },
		func(c *Contract) { c.WorkspaceID = "other" },
		func(c *Contract) { c.SymbolID = "repo/other" },
	} {
		changed := base
		mutate(&changed)
		fingerprint, err := FingerprintRecords([]Contract{changed})
		require.NoError(t, err)
		require.NotEqual(t, initial.Semantic, fingerprint.Semantic)
	}
	duplicate, err := FingerprintRecords([]Contract{base, base})
	require.NoError(t, err)
	require.NotEqual(t, initial.Full, duplicate.Full)
	_, err = FingerprintRecords([]Contract{{Confidence: math.NaN()}})
	require.Error(t, err, "unserializable records cannot certify equality")
}

func TestContractRecordFingerprintsIgnoreListingOrder(t *testing.T) {
	one := Contract{ID: "shared", Role: RoleProvider, FilePath: "a.go"}
	two := Contract{ID: "shared", Role: RoleConsumer, FilePath: "b.go"}
	left, err := FingerprintRecords([]Contract{one, two})
	require.NoError(t, err)
	right, err := FingerprintRecords([]Contract{two, one})
	require.NoError(t, err)
	require.Equal(t, left, right)
}

func TestContractRecordFingerprintIncludesEverySemanticField(t *testing.T) {
	base := Contract{ID: "id", Type: ContractHTTP, Role: RoleProvider, SymbolID: "symbol", FilePath: "file.go", Line: 9, RepoPrefix: "repo", WorkspaceID: "workspace", ProjectID: "project", Confidence: .9, Meta: map[string]any{"nested": map[string]any{"line": 12, "schema": []any{"a", "b"}}}}
	initial, err := FingerprintRecords([]Contract{base})
	require.NoError(t, err)
	cases := map[string]func(*Contract){
		"id":         func(c *Contract) { c.ID = "other" },
		"type":       func(c *Contract) { c.Type = ContractGRPC },
		"role":       func(c *Contract) { c.Role = RoleConsumer },
		"symbol":     func(c *Contract) { c.SymbolID = "other" },
		"file":       func(c *Contract) { c.FilePath = "other.go" },
		"repo":       func(c *Contract) { c.RepoPrefix = "other" },
		"workspace":  func(c *Contract) { c.WorkspaceID = "other" },
		"project":    func(c *Contract) { c.ProjectID = "other" },
		"confidence": func(c *Contract) { c.Confidence = .8 },
		"nested_line": func(c *Contract) {
			c.Meta = map[string]any{"nested": map[string]any{"line": 13, "schema": []any{"a", "b"}}}
		},
		"nested_array_order": func(c *Contract) {
			c.Meta = map[string]any{"nested": map[string]any{"line": 12, "schema": []any{"b", "a"}}}
		},
		"extra_metadata": func(c *Contract) { c.Meta = map[string]any{"nested": base.Meta["nested"], "extra": true} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			got, err := FingerprintRecords([]Contract{changed})
			require.NoError(t, err)
			require.NotEqual(t, initial.Semantic, got.Semantic)
			require.NotEqual(t, initial.Full, got.Full)
		})
	}
}

func TestContractRecordFingerprintMapOrderAndInputPreservation(t *testing.T) {
	metaA := map[string]any{}
	metaA["z"] = []any{1, "x"}
	metaA["a"] = map[string]any{"y": 2, "b": 3}
	metaB := map[string]any{}
	metaB["a"] = map[string]any{"b": 3, "y": 2}
	metaB["z"] = []any{1, "x"}
	input := []Contract{{ID: "one", Line: 9, Meta: metaA}, {ID: "two", Line: 2}}
	before, err := json.Marshal(input)
	require.NoError(t, err)
	left, err := FingerprintRecords(input)
	require.NoError(t, err)
	right, err := FingerprintRecords([]Contract{{ID: "two", Line: 2}, {ID: "one", Line: 9, Meta: metaB}})
	require.NoError(t, err)
	require.Equal(t, left, right)
	after, err := json.Marshal(input)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, "one", input[0].ID)
	require.Equal(t, 9, input[0].Line)
}

func TestContractRecordFingerprintMultiplicityAndEmpty(t *testing.T) {
	empty, err := FingerprintRecords(nil)
	require.NoError(t, err)
	emptySlice, err := FingerprintRecords([]Contract{})
	require.NoError(t, err)
	require.Equal(t, empty, emptySlice)
	require.Len(t, empty.Semantic, 64)
	one, err := FingerprintRecords([]Contract{{ID: "same"}})
	require.NoError(t, err)
	duplicate, err := FingerprintRecords([]Contract{{ID: "same"}, {ID: "same"}})
	require.NoError(t, err)
	require.NotEqual(t, one.Semantic, duplicate.Semantic)
	require.NotEqual(t, one.Full, duplicate.Full)
	require.NotEqual(t, empty.Semantic, one.Semantic)
	require.NotEqual(t, empty.Full, one.Full)
}

func TestContractRecordFingerprintSerializationFailsClosed(t *testing.T) {
	cycle := map[string]any{}
	cycle["self"] = cycle
	for name, bad := range map[string]Contract{"nan": {Confidence: math.NaN()}, "function": {Meta: map[string]any{"bad": func() {}}}, "cycle": {Meta: cycle}} {
		t.Run(name, func(t *testing.T) {
			got, err := FingerprintRecords([]Contract{{ID: "valid"}, bad})
			require.Error(t, err)
			require.Equal(t, RecordFingerprints{}, got, "partial hashes must never certify equality")
		})
	}
}

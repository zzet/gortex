package main

import "testing"

func TestRPCErrorDataSnapshotOwnsNestedJSON(t *testing.T) {
	original := map[string]any{"code": "view_pending", "details": map[string]any{"paths": []any{"one.go", map[string]any{"scope": "checkout"}}}}
	snapshot := copyRPCErrorData(original)
	original["code"] = "changed"
	details := original["details"].(map[string]any)
	details["paths"].([]any)[0] = "two.go"
	details["paths"].([]any)[1].(map[string]any)["scope"] = "other"
	kept := snapshot["details"].(map[string]any)["paths"].([]any)
	if snapshot["code"] != "view_pending" || kept[0] != "one.go" || kept[1].(map[string]any)["scope"] != "checkout" {
		t.Fatalf("snapshot borrowed decoded RPC data: %+v", snapshot)
	}
	if copyRPCErrorData(nil) != nil {
		t.Fatal("nil data became a fabricated object")
	}
}

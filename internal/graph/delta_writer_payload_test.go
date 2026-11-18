package graph

import (
	"reflect"
	"testing"
)

// The payload's edge comparison accounts for every field of Edge; a new field
// must be taught to it (or it silently compares by rendering, which is slow).
func TestDeltaEdgeComparisonAccountsForEveryField(t *testing.T) {
	if got := reflect.TypeOf(Edge{}).NumField(); got != deltaEdgeFieldsCompared {
		t.Fatalf("Edge has %d fields; deltaEdgeContentEqual accounts for %d", got, deltaEdgeFieldsCompared)
	}
	a := &Edge{From: "a", To: "b", Kind: EdgeCalls, FilePath: "f", Line: 1, Meta: map[string]any{"arg_names": []string{"x"}}}
	b := &Edge{From: "a", To: "b", Kind: EdgeCalls, FilePath: "f", Line: 1, Meta: map[string]any{"arg_names": []any{"x"}}}
	if !deltaEdgeSetsEqual([]*Edge{a}, []*Edge{b}) {
		t.Fatal("a decoded list and an engine list with the same items must compare equal")
	}
	c := *b
	c.Origin = "ast_resolved"
	if deltaEdgeSetsEqual([]*Edge{a}, []*Edge{&c}) {
		t.Fatal("a changed origin must compare unequal")
	}
	d := *a
	d.Context = "ignored"
	if !deltaEdgeSetsEqual([]*Edge{a}, []*Edge{&d}) {
		t.Fatal("fields a store does not persist must not count")
	}
}

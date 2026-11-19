package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestSvelteExtractor(t *testing.T) {
	const svelte = `<script data-note="x > y" lang=ts>
  import { onMount } from 'svelte'
  let count = 0
  function increment(): void { count += 1 }
</script>

<button on:click={increment}>{count}</button>
`
	res, err := NewSvelteExtractor().Extract("Counter.svelte", []byte(svelte))
	if err != nil {
		t.Fatal(err)
	}

	var comp, incr *graph.Node
	for _, n := range res.Nodes {
		if n.Kind == graph.KindType && n.Name == "Counter" {
			comp = n
		}
		if n.Name == "increment" {
			incr = n
		}
	}
	if comp == nil {
		t.Fatalf("no component node 'Counter' among %d nodes", len(res.Nodes))
	}
	if comp.Meta["exported"] != true || comp.Language != "svelte" {
		t.Errorf("component meta/lang = %v / %q", comp.Meta, comp.Language)
	}
	if incr == nil {
		t.Fatalf("delegated function 'increment' was not extracted from <script>")
	}
	if incr.Language != "svelte" || incr.Meta["inline_script"] != true {
		t.Errorf("delegated symbol lang=%q meta=%v, want svelte + inline_script", incr.Language, incr.Meta)
	}
	if incr.StartLine != 4 {
		t.Errorf("increment StartLine = %d, want 4 (host-file coordinates)", incr.StartLine)
	}
}

func TestSvelteExtractor_StoreSubscription(t *testing.T) {
	const svelte = `<script>
  import { count, name } from './store';
  let local = 1;
</script>

<p>{$count}</p>
<h1>{$name}</h1>
<button>{local}</button>
`
	res, err := NewSvelteExtractor().Extract("Counter.svelte", []byte(svelte))
	if err != nil {
		t.Fatal(err)
	}
	stores := map[string]string{}
	for _, e := range res.Edges {
		if e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "svelte_store" {
			continue
		}
		s, _ := e.Meta["store"].(string)
		stores[s] = e.To
		if e.From != "Counter.svelte::Counter" {
			t.Errorf("store subscription should be attributed to the component, got %q", e.From)
		}
		if e.Kind != graph.EdgeReferences {
			t.Errorf("store subscription should be an EdgeReferences, got %v", e.Kind)
		}
	}
	if stores["count"] != "unresolved::import::./store::count" {
		t.Errorf("$count should resolve to the count store import, got %q", stores["count"])
	}
	if stores["name"] != "unresolved::import::./store::name" {
		t.Errorf("$name should resolve to the name store import, got %q", stores["name"])
	}
}

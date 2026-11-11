package contracts

import "testing"

func laravelContractIDs(cs []Contract) map[string]Contract {
	out := map[string]Contract{}
	for _, c := range cs {
		if fw, _ := c.Meta["framework"].(string); fw == "laravel" {
			out[c.ID] = c
		}
	}
	return out
}

func TestLaravelResource_FullExpansion(t *testing.T) {
	src := []byte(`<?php
Route::resource('users', UserController::class);
`)
	ext := &HTTPExtractor{}
	byID := laravelContractIDs(ext.Extract("routes/web.php", src, nil, nil))
	want := []string{
		"http::GET::/users", "http::GET::/users/create", "http::POST::/users",
		"http::GET::/users/{p1}", "http::GET::/users/{p1}/edit",
		"http::PUT::/users/{p1}", "http::DELETE::/users/{p1}",
	}
	if len(byID) != 7 {
		t.Fatalf("Route::resource should expand to 7 routes, got %d: %v", len(byID), keysOf(byID))
	}
	for _, id := range want {
		c, ok := byID[id]
		if !ok {
			t.Errorf("missing route %s", id)
			continue
		}
		if hc, _ := c.Meta["handler_class"].(string); hc != "UserController" {
			t.Errorf("%s handler_class = %q (want UserController)", id, hc)
		}
	}
}

func TestLaravelResource_ApiResourceDropsCreateEdit(t *testing.T) {
	src := []byte(`<?php
Route::apiResource('posts', PostController::class);
`)
	ext := &HTTPExtractor{}
	byID := laravelContractIDs(ext.Extract("routes/api.php", src, nil, nil))
	if len(byID) != 5 {
		t.Fatalf("apiResource should expand to 5 routes (no create/edit), got %d: %v", len(byID), keysOf(byID))
	}
	if _, ok := byID["http::GET::/posts/create"]; ok {
		t.Errorf("apiResource must not produce the create route")
	}
	if _, ok := byID["http::GET::/posts/{p1}/edit"]; ok {
		t.Errorf("apiResource must not produce the edit route")
	}
}

func TestLaravelResource_OnlyFilter(t *testing.T) {
	src := []byte(`<?php
Route::resource('tags', TagController::class)->only(['index', 'show']);
`)
	ext := &HTTPExtractor{}
	byID := laravelContractIDs(ext.Extract("routes/web.php", src, nil, nil))
	if len(byID) != 2 {
		t.Fatalf("->only(['index','show']) should yield 2 routes, got %d: %v", len(byID), keysOf(byID))
	}
	if _, ok := byID["http::GET::/tags"]; !ok {
		t.Errorf("missing index route")
	}
	if _, ok := byID["http::GET::/tags/{p1}"]; !ok {
		t.Errorf("missing show route")
	}
}

func TestLaravelResource_ExceptFilter(t *testing.T) {
	src := []byte(`<?php
Route::resource('items', ItemController::class)->except(['destroy', 'edit', 'create']);
`)
	ext := &HTTPExtractor{}
	byID := laravelContractIDs(ext.Extract("routes/web.php", src, nil, nil))
	if _, ok := byID["http::DELETE::/items/{p1}"]; ok {
		t.Errorf("->except must drop the destroy route")
	}
	if len(byID) != 4 {
		t.Errorf("except(destroy,edit,create) should leave 4 routes, got %d: %v", len(byID), keysOf(byID))
	}
}

func TestLaravelResource_Options(t *testing.T) {
	for _, tc := range []struct {
		name, route string
		want        []string
	}{
		{"resource", `Route::resource("orders", ShopController::class, ["names" => "orders"])->only(["index"]);`, []string{"http::GET::/orders"}},
		{"api_resource", `Route::apiResource("orders", ShopController::class, ["names" => ["show" => "orders.show"]])->except(["destroy", "update"]);`, []string{"http::GET::/orders", "http::POST::/orders", "http::GET::/orders/{p1}"}},
		{"nested_options", `Route::resource("orders", ShopController::class, ["names" => array("index" => "orders.index)"), "parameters" => ["orders" => "order"]])->only(["index"]);`, []string{"http::GET::/orders"}},
		{"options_only", `Route::resource("orders", ShopController::class, ["only" => ["index"], "names" => ["show" => "orders.show"]]);`, []string{"http::GET::/orders"}},
		{"options_except", `Route::apiResource("orders", ShopController::class, ["except" => ["destroy", "update"]])->only(["index", "destroy"]);`, []string{"http::GET::/orders"}},
		{"nested_filter_name", `Route::resource("orders", ShopController::class, ["names" => ["only" => "ignored"]])->only(["index"]);`, []string{"http::GET::/orders"}},
		{"trailing_comma", `Route::resource("orders", ShopController::class, ["names" => "orders"],)->only(["index"]);`, []string{"http::GET::/orders"}},
		{"dynamic_options", `Route::resource("orders", ShopController::class, $options);`, nil},
		{"dynamic_filter", `Route::resource("orders", ShopController::class, ["except" => $actions]);`, nil},
		{"spread_options", `Route::resource("orders", ShopController::class, [...$options]);`, nil},
		{"multiline_filter", `Route::resource("orders", ShopController::class, ["only" => [
    "index",
  ]]);`, []string{"http::GET::/orders"}},
		{"empty_options", `Route::resource("orders", ShopController::class, [  ])->only(["index"]);`, []string{"http::GET::/orders"}},
		{"multiline_chain", `Route::resource("orders", ShopController::class, ["names" => "orders"])
    ->only(["index"]);`, []string{"http::GET::/orders"}},
		{"dynamic_chain", `Route::resource("orders", ShopController::class, ["names" => "orders"])->only($actions);`, nil},
		{"chain_override", `Route::resource("orders", ShopController::class, ["only" => ["show"]])->only(["show"])->only(["index"]);`, []string{"http::GET::/orders"}},
		{"no_import", `Route::resource("orders", ShopController::class)->only(["index"]);`, []string{"http::GET::/orders"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte("<?php\n" + tc.route + "\nRoute::resource('other', CatalogController::class)->only(['show']);\n")
			got := laravelContractIDs((&HTTPExtractor{}).Extract("routes/api.php", src, nil, nil))
			if len(got) != len(tc.want)+1 {
				t.Fatalf("want %d routes including the independent registration, got %d: %v", len(tc.want)+1, len(got), keysOf(got))
			}
			for _, id := range tc.want {
				c, ok := got[id]
				if !ok {
					t.Errorf("missing %s", id)
					continue
				}
				if c.SymbolID != "" || c.Meta["handler_class"] != "ShopController" {
					t.Errorf("unexpected handler for %s: %+v", id, c)
				}
				if c.Line != 2 {
					t.Errorf("line = %d, want 2", c.Line)
				}
			}
			if c := got["http::GET::/other/{p1}"]; c.Meta["handler_class"] != "CatalogController" {
				t.Errorf("independent registration changed: %+v", c)
			}
		})
	}
}

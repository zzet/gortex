package indexer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/config"
)

func TestDedicatedBaseConfigSnapshotOwnsEveryField(t *testing.T) {
	var original config.IndexConfig
	fillDedicatedConfigTestValue(t, reflect.ValueOf(&original).Elem(), "IndexConfig")
	before := dedicatedConfigTestJSON(t, original)
	owned, fingerprint, err := snapshotDedicatedBaseConfig(original, "repo", "workspace", "project")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, owned) {
		t.Fatal("JSON snapshot did not preserve every populated IndexConfig field")
	}
	if !bytes.Equal(before, dedicatedConfigTestJSON(t, original)) {
		t.Fatal("snapshot mutated the caller's configuration")
	}
	// Mutate in place, not by replacing slices/pointers: this covers nested
	// SkipRule.Kinds, transform/plugin/grammar/chunker slices, coverage
	// reference fields, every *bool, and FrameworkSynthesizers ownership.
	mutateDedicatedConfigTestValue(t, reflect.ValueOf(&original).Elem())
	if !bytes.Equal(before, dedicatedConfigTestJSON(t, owned)) {
		t.Fatal("caller mutation changed the owned configuration")
	}
	_, repeated, err := snapshotDedicatedBaseConfig(owned, "repo", "workspace", "project")
	if err != nil || repeated != fingerprint {
		t.Fatalf("caller mutation changed captured fingerprint: %q, %v", repeated, err)
	}
	originalAfter := dedicatedConfigTestJSON(t, original)
	mutateDedicatedConfigTestValue(t, reflect.ValueOf(&owned).Elem())
	if !bytes.Equal(originalAfter, dedicatedConfigTestJSON(t, original)) {
		t.Fatal("snapshot mutation changed the caller's configuration")
	}
}

func TestDedicatedBaseConfigFrameworkSelection(t *testing.T) {
	var nilList []string
	empty := []string{}
	selection := []string{"zeta", "alpha", "zeta"}
	equivalent := []string{"alpha", "zeta"}
	other := []string{"beta"}
	cases := []struct {
		name string
		list *[]string
	}{
		{"default", nil}, {"nil_list", &nilList}, {"empty", &empty},
		{"selected", &selection}, {"equivalent", &equivalent}, {"other", &other},
	}
	hashes := make(map[string]string)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.IndexConfig{FrameworkSynthesizers: tc.list}
			owned, hash, err := snapshotDedicatedBaseConfig(cfg, "repo", "workspace", "project")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg, owned) {
				t.Fatal("snapshot changed nil/empty/order/duplicate semantics")
			}
			if tc.list != nil && owned.FrameworkSynthesizers == tc.list {
				t.Fatal("framework pointer is borrowed")
			}
			hashes[tc.name] = hash
		})
	}
	if hashes["default"] == hashes["empty"] || hashes["default"] == hashes["nil_list"] {
		t.Fatal("default registry and explicitly disabled framework pipeline collided")
	}
	if hashes["nil_list"] != hashes["empty"] || hashes["selected"] != hashes["equivalent"] {
		t.Fatal("semantically identical framework sets have different fingerprints")
	}
	if hashes["selected"] == hashes["other"] || hashes["selected"] == hashes["empty"] {
		t.Fatal("different framework sets collided")
	}
	if !reflect.DeepEqual(selection, []string{"zeta", "alpha", "zeta"}) {
		t.Fatal("fingerprint normalization changed caller order")
	}
}

func TestDedicatedBaseConfigFingerprintOutputContext(t *testing.T) {
	cfg := config.Default().Index
	contexts := [][3]string{
		{"repo", "workspace", "project"},
		{"other", "workspace", "project"},
		{"repo", "other", "project"},
		{"repo", "workspace", "other"},
		{"ab", "c", "d"}, {"a", "bc", "d"},
	}
	seen := make(map[string][3]string)
	for _, context := range contexts {
		_, hash, err := snapshotDedicatedBaseConfig(cfg, context[0], context[1], context[2])
		if err != nil {
			t.Fatal(err)
		}
		if len(hash) != 64 {
			t.Fatalf("want SHA256 fingerprint, got %q", hash)
		}
		if prior, ok := seen[hash]; ok {
			t.Fatalf("output contexts collided: %q and %q", prior, context)
		}
		seen[hash] = context
		_, repeated, err := snapshotDedicatedBaseConfig(cfg, context[0], context[1], context[2])
		if err != nil || repeated != hash {
			t.Fatalf("unstable context fingerprint: %q, %v", repeated, err)
		}
	}
}

func TestDedicatedBaseConfigFingerprintEffectiveFields(t *testing.T) {
	baseline := config.IndexConfig{
		IndexProse: true,
		SkipEmbed:  []config.SkipEmbedRule{{Language: "go", Kinds: []string{"variable"}}},
		SkipSearch: []config.SkipEmbedRule{{Language: "json", Kinds: []string{"variable"}}},
	}
	_, initial, err := snapshotDedicatedBaseConfig(baseline, "repo", "workspace", "project")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*config.IndexConfig)
	}{
		{"index_prose", func(cfg *config.IndexConfig) { cfg.IndexProse = false }},
		{"skip_embed", func(cfg *config.IndexConfig) { cfg.SkipEmbed[0].Kinds[0] = "type" }},
		{"skip_search", func(cfg *config.IndexConfig) { cfg.SkipSearch[0].Kinds[0] = "type" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, err := snapshotDedicatedBaseConfig(baseline, "repo", "workspace", "project")
			if err != nil {
				t.Fatal(err)
			}
			tc.change(&cfg)
			_, changed, err := snapshotDedicatedBaseConfig(cfg, "repo", "workspace", "project")
			if err != nil || changed == initial {
				t.Fatalf("effective field omitted from fingerprint: %q, %v", changed, err)
			}
		})
	}
}

func TestDedicatedBaseConfigJSONShapeGuard(t *testing.T) {
	if err := dedicatedConfigJSONShape(reflect.TypeOf(config.IndexConfig{}), "IndexConfig"); err != nil {
		t.Fatal(err)
	}
	// Positive controls keep this guard from silently becoming an empty walk.
	omitted := struct {
		Value []string `json:"-"`
	}{}
	if err := dedicatedConfigJSONShape(reflect.TypeOf(omitted), "Omitted"); err == nil {
		t.Fatal("guard accepted a JSON-omitted config field")
	}
	nested := struct {
		Child []struct {
			Value bool `json:"-"`
		}
	}{}
	if err := dedicatedConfigJSONShape(reflect.TypeOf(nested), "Nested"); err == nil {
		t.Fatal("guard accepted a nested JSON-omitted config field")
	}
	ambiguous := struct {
		Selection *[]string
	}{}
	if err := dedicatedConfigJSONShape(reflect.TypeOf(ambiguous), "Ambiguous"); err == nil {
		t.Fatal("guard accepted a new pointer-to-slice null ambiguity")
	}
}

// Reflection is restricted to test fixture generation, in-place mutation, and
// guarding serialization completeness. Production cloning uses encoding/json.
func dedicatedConfigJSONShape(typ reflect.Type, path string) error {
	switch typ.Kind() {
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := path + "." + field.Name
			if field.PkgPath != "" || strings.Split(field.Tag.Get("json"), ",")[0] == "-" {
				return fmt.Errorf("%s is omitted by JSON; update dedicated snapshot ownership", name)
			}
			if err := dedicatedConfigJSONShape(field.Type, name); err != nil {
				return err
			}
		}
	case reflect.Pointer:
		kind := typ.Elem().Kind()
		if (kind == reflect.Slice || kind == reflect.Map || kind == reflect.Pointer) && path != "IndexConfig.FrameworkSynthesizers" {
			return fmt.Errorf("%s needs explicit JSON-null preservation", path)
		}
		return dedicatedConfigJSONShape(typ.Elem(), path)
	case reflect.Slice, reflect.Array:
		return dedicatedConfigJSONShape(typ.Elem(), path+"[]")
	case reflect.Map:
		if typ.Key().Kind() != reflect.String {
			return fmt.Errorf("%s has an unsupported JSON map key", path)
		}
		return dedicatedConfigJSONShape(typ.Elem(), path+"{}")
	case reflect.Bool, reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
	default:
		return fmt.Errorf("%s has unsupported snapshot kind %s", path, typ.Kind())
	}
	return nil
}

func fillDedicatedConfigTestValue(t testing.TB, value reflect.Value, path string) {
	t.Helper()
	switch value.Kind() {
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			fillDedicatedConfigTestValue(t, value.Field(i), path+"."+value.Type().Field(i).Name)
		}
	case reflect.Pointer:
		value.Set(reflect.New(value.Type().Elem()))
		fillDedicatedConfigTestValue(t, value.Elem(), path)
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice {
			value.Set(reflect.MakeSlice(value.Type(), 2, 2))
		}
		for i := 0; i < value.Len(); i++ {
			fillDedicatedConfigTestValue(t, value.Index(i), fmt.Sprintf("%s[%d]", path, i))
		}
	case reflect.Map:
		value.Set(reflect.MakeMap(value.Type()))
		key := reflect.New(value.Type().Key()).Elem()
		fillDedicatedConfigTestValue(t, key, path+".key")
		entry := reflect.New(value.Type().Elem()).Elem()
		fillDedicatedConfigTestValue(t, entry, path+".value")
		value.SetMapIndex(key, entry)
	case reflect.String:
		value.SetString(path)
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value.SetUint(7)
	case reflect.Float32, reflect.Float64:
		value.SetFloat(0.25)
	default:
		t.Fatalf("add all-fields fixture support for %s: %s", path, value.Kind())
	}
}

func mutateDedicatedConfigTestValue(t testing.TB, value reflect.Value) {
	t.Helper()
	switch value.Kind() {
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			mutateDedicatedConfigTestValue(t, value.Field(i))
		}
	case reflect.Pointer:
		mutateDedicatedConfigTestValue(t, value.Elem())
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			mutateDedicatedConfigTestValue(t, value.Index(i))
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			entry := reflect.New(value.Type().Elem()).Elem()
			entry.Set(iter.Value())
			mutateDedicatedConfigTestValue(t, entry)
			value.SetMapIndex(iter.Key(), entry)
		}
	case reflect.String:
		value.SetString(value.String() + "_changed")
	case reflect.Bool:
		value.SetBool(!value.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(value.Int() + 1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value.SetUint(value.Uint() + 1)
	case reflect.Float32, reflect.Float64:
		value.SetFloat(value.Float() + 1)
	default:
		t.Fatalf("add in-place mutation support for %s", value.Kind())
	}
}

func dedicatedConfigTestJSON(t testing.TB, cfg config.IndexConfig) []byte {
	t.Helper()
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

var dedicatedConfigBenchmarkSnapshot config.IndexConfig
var dedicatedConfigBenchmarkFingerprint string

func BenchmarkDedicatedBaseConfigSnapshot(b *testing.B) {
	configured := config.Default().Index
	configured.Languages = []string{"go", "typescript"}
	configured.Exclude = []string{"vendor/", "node_modules/", "dist/"}
	frameworks := []string{"http", "grpc", "http"}
	configured.FrameworkSynthesizers = &frameworks
	configured.Transforms = []config.TransformRule{{Name: "template", Extensions: []string{".tmpl"}, Command: []string{"normalize-template", "--stdin"}, AsLanguage: "html"}}
	configured.Grammars = []config.GrammarSpec{{Language: "custom", Library: "grammar.so", Extensions: []string{".custom"}}}
	configured.ExtractorPlugins = []config.ExtractorPluginSpec{{Language: "custom", Extensions: []string{".custom"}, Command: "extract", Args: []string{"--json"}}}
	for _, tc := range []struct {
		name string
		cfg  config.IndexConfig
	}{{"default", config.Default().Index}, {"configured", configured}} {
		b.Run(tc.name, func(b *testing.B) {
			_, expected, err := snapshotDedicatedBaseConfig(tc.cfg, "repo", "workspace", "project")
			if err != nil {
				b.Fatal(err)
			}
			_, differentContext, err := snapshotDedicatedBaseConfig(tc.cfg, "repo", "other-workspace", "project")
			if err != nil || differentContext == expected {
				b.Fatal("benchmark context oracle failed", err)
			}
			configBytes := len(dedicatedConfigTestJSON(b, tc.cfg))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				owned, fingerprint, err := snapshotDedicatedBaseConfig(tc.cfg, "repo", "workspace", "project")
				if err != nil || fingerprint != expected {
					b.Fatal("benchmark snapshot/hash oracle failed", err)
				}
				dedicatedConfigBenchmarkSnapshot = owned
				dedicatedConfigBenchmarkFingerprint = fingerprint
			}
			b.StopTimer()
			b.ReportMetric(float64(configBytes), "config-bytes")
		})
	}
}

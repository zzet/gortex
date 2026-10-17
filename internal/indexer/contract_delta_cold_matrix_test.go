package indexer

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// These rows extend the existing route/producer/fill tests to the entire
// published payload. Inputs reuse the HTTP reply/client, @Inject and Spring
// bean shapes from contract_attachment_worker_test.go, through the real
// primary journal/materializer fixture instead of a synthetic receipt source.
type contractColdMatrixFixture struct {
	name       string
	files      map[string]string
	valueEdits map[string]string
	function   map[string]string
	contracts  []string
	shape      bool
	bridge     bool
	spring     bool
}

func contractColdMatrixFixtures() []contractColdMatrixFixture {
	server := "package server\nimport \"net/http\"\ntype Reply struct { Name string `json:\"name\"` }\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET /v1/items\", handle) }\nfunc handle(w http.ResponseWriter, r *http.Request) { response := Reply{Name:\"ok\"}; WriteJSON(w,200,response) }\nfunc WriteJSON(w http.ResponseWriter, code int, body any) {}\n"
	client := "package server\nimport \"net/http\"\nfunc call() { http.Get(\"http://example.test/v1/items\") }\n"
	di := "import { Module, Injectable, Inject } from '@nestjs/common';\nconst DATABASE_URL = 'DATABASE_URL';\n@Module({ providers: [{ provide: DATABASE_URL, useValue: 'postgres://accepted' }] })\nexport class ConfigModule {}\n@Injectable()\nexport class ConfigService { constructor(@Inject(DATABASE_URL) private readonly dbUrl: string) {} }\n"
	bean := "import java.time.Clock; import org.springframework.context.annotation.Bean; import org.springframework.context.annotation.Configuration;\n@Configuration public class Clocks { @Bean public Clock systemClock() { return Clock.systemUTC(); } }\n"
	service := "import java.time.Clock; import org.springframework.beans.factory.annotation.Autowired; import org.springframework.stereotype.Service;\n@Service public class OrderService { @Autowired public OrderService(Clock clock) {} }\n"
	config := "package fixture\nimport \"os\"\nfunc Config() string { return os.Getenv(\"MATRIX_DATABASE_URL\") }\n"
	return []contractColdMatrixFixture{
		{name: "http_shape_bridge", files: map[string]string{"server.go": server, "client.go": client}, valueEdits: map[string]string{
			"server.go": strings.ReplaceAll(server, "Name string `json:\"name\"`", "Name string `json:\"name\"`; Count int `json:\"count\"`"),
			"client.go": strings.ReplaceAll(client, "http.Get", "http.Head"),
		}, function: map[string]string{"server.go": "\nfunc MatrixAdded() int { return 1 }\n", "client.go": "\nfunc MatrixAdded() int { return 1 }\n"}, contracts: []string{"http::GET::/v1/items"}, shape: true, bridge: true},
		{name: "config_owner", files: map[string]string{"config.go": config}, valueEdits: map[string]string{"config.go": strings.ReplaceAll(config, "MATRIX_DATABASE_URL", "MATRIX_DATABASE_URL_NEXT")}, function: map[string]string{"config.go": "\nfunc MatrixAdded() int { return 1 }\n"}},
		{name: "typescript_di", files: map[string]string{"config.ts": di}, valueEdits: map[string]string{"config.ts": strings.ReplaceAll(di, "DATABASE_URL", "RENAMED_DATABASE_URL")}, function: map[string]string{"config.ts": "\nexport function matrixAdded(): number { return 1; }\n"}, contracts: []string{"di::DATABASE_URL"}},
		{name: "spring_bean", files: map[string]string{"Clocks.java": bean, "OrderService.java": service}, valueEdits: map[string]string{
			"Clocks.java":       strings.ReplaceAll(bean, "systemClock", "renamedClock"),
			"OrderService.java": strings.ReplaceAll(service, "Clock clock", "Clock renamedClock"),
		}, function: map[string]string{
			"Clocks.java":       strings.Replace(bean, "{ @Bean", "{ public int matrixAdded() { return 1; } @Bean", 1),
			"OrderService.java": strings.Replace(service, "{ @Autowired", "{ public int matrixAdded() { return 1; } @Autowired", 1),
		}, spring: true},
	}
}

func contractColdMatrixRegistry() *parser.Registry {
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	// Failure rows use the existing deterministic extractor failure rather
	// than platform permissions or a parser's tolerance for malformed text.
	registry.Register(contractFailingGoExtractor{GoExtractor: languages.NewGoExtractor()})
	return registry
}

type contractColdMatrixSnapshot struct {
	Nodes, Edges, Owners, Documents []string
	Normalization                   string
}

// Edge's ordinary JSON deliberately excludes Meta and Confidence. Include
// both explicitly; owner roles, derived call provenance and payload metadata
// are part of this oracle, rather than just canonical contract identities.
func contractColdMatrixEdge(edge *graph.Edge) any {
	return struct {
		Edge       *graph.Edge
		Meta       map[string]any
		Confidence float64
	}{edge, edge.Meta, edge.Confidence}
}

func contractColdMatrixJSON(t *testing.T, root string, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	require.NoError(t, err)
	// The independent checkout has a distinct temporary absolute root. No
	// semantic fields, line numbers, ownership or source metadata are erased.
	result := string(b)
	for _, spelling := range []string{root, filepath.ToSlash(root)} {
		encodedRoot, err := json.Marshal(spelling)
		require.NoError(t, err)
		result = strings.ReplaceAll(result, string(encodedRoot[1:len(encodedRoot)-1]), "<fixture-root>")
	}
	return result
}

func contractColdMatrixRead(t *testing.T, f *contractJobFixture) contractColdMatrixSnapshot {
	t.Helper()
	ctx := t.Context()
	inputs, err := f.options.Materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	analysis, err := f.options.Materializer.OpenContractAnalysisForInputs(ctx, nil, inputs)
	require.NoError(t, err)
	defer analysis.Close()
	nodes, err := analysis.NodesContext(ctx, graph.ContractProjectionRowLimit)
	require.NoError(t, err)
	var out contractColdMatrixSnapshot
	var ids []string
	for _, node := range nodes {
		ids = append(ids, node.ID)
		out.Nodes = append(out.Nodes, contractColdMatrixJSON(t, f.root, node))
	}
	edges, err := analysis.OutEdgesContext(ctx, ids, graph.ContractProjectionRowLimit)
	require.NoError(t, err)
	for _, list := range edges {
		for _, edge := range list {
			out.Edges = append(out.Edges, contractColdMatrixJSON(t, f.root, contractColdMatrixEdge(edge)))
		}
	}
	// Read the exact leased payload, not a latest generation or core registry.
	payload := f.store.AtGeneration(analysis.Attachment.PayloadGeneration)
	owners, err := payload.LayerContractRepoProjectionContext(ctx, "fixture")
	require.NoError(t, err)
	for _, row := range owners.OwnerRows {
		out.Owners = append(out.Owners, contractColdMatrixJSON(t, f.root, struct {
			Repo string
			Edge any
		}{row.RepoPrefix, contractColdMatrixEdge(row.Edge)}))
	}
	for after := int64(0); ; {
		rows, next, err := f.store.SymbolFTSGenerationRows(ctx, analysis.Attachment.PayloadGeneration, "", after, 7)
		require.NoError(t, err)
		for _, row := range rows {
			row.RowID = 0 // physical allocation identity is not search semantics
			out.Documents = append(out.Documents, contractColdMatrixJSON(t, f.root, row))
		}
		if next == 0 {
			break
		}
		require.Greater(t, next, after)
		after = next
	}
	count, err := payload.SymbolFTSCount()
	require.NoError(t, err)
	require.Len(t, out.Documents, count, "pagination must include every actual document")
	mode, known, err := payload.GetSymbolFTSNormalization("fixture")
	require.NoError(t, err)
	require.True(t, known)
	require.Equal(t, search.FTSNormalizationMode(), mode)
	out.Normalization = mode
	for _, rows := range [][]string{out.Nodes, out.Edges, out.Owners, out.Documents} {
		slices.Sort(rows)
	}
	require.NoError(t, analysis.Validate(ctx))
	return out
}

func contractColdMatrixCold(t *testing.T, files map[string]string) contractColdMatrixSnapshot {
	t.Helper()
	cold := newContractJobFixtureWithRegistry(t, files, contractColdMatrixRegistry())
	report, err := cold.job(nil, nil) // no retained P, no shared store or core
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, viewmetrics.ContractModeCold, report.Mode)
	return contractColdMatrixRead(t, cold)
}

func contractColdMatrixPremise(t *testing.T, f *contractJobFixture, row contractColdMatrixFixture) {
	t.Helper()
	snapshot := contractColdMatrixRead(t, f)
	require.NotEmpty(t, snapshot.Nodes)
	require.NotEmpty(t, snapshot.Owners)
	require.NotEmpty(t, snapshot.Documents)
	for _, id := range row.contracts {
		require.Contains(t, contractIDs(f.contracts()), id)
	}
	joined := strings.Join(snapshot.Nodes, "\n")
	if row.shape {
		require.Contains(t, joined, `"shape":`, "reply shape output must be non-vacuous")
	}
	if row.bridge {
		require.Contains(t, joined, `"kind":"contract_bridge"`, "both HTTP sides must actually match")
	}
	if row.spring {
		require.Contains(t, strings.Join(snapshot.Edges, "\n"), "spring.Bean", "derived call output must be non-vacuous")
	}
}

func contractColdMatrixEdit(row contractColdMatrixFixture, path, operation string) string {
	switch operation {
	case "comment":
		return row.files[path] + "\n// matrix comment\n"
	case "new_function":
		if strings.HasSuffix(path, ".java") {
			return row.function[path]
		}
		return row.files[path] + row.function[path]
	case "value_or_owner":
		return row.valueEdits[path]
	default:
		panic("unsupported matrix edit: " + operation)
	}
}

func contractColdMatrixAttachment(t *testing.T, f *contractJobFixture) graph.ContractAttachment {
	t.Helper()
	inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
	require.NoError(t, err)
	key := graph.ContractAttachmentKey{RepoPrefix: "fixture", InputVersion: inputs.State.InputVersion, InputFingerprint: inputs.State.InputFingerprint}
	header, err := f.store.GetContractAttachmentContext(t.Context(), key)
	require.NoError(t, err)
	require.NotNil(t, header)
	return *header
}

// A source-only edit can carry the existing contract identity and owe no job.
// Permit that path only when an independent cold build proves the entire
// output unchanged; a lost meaningful boundary change must still fail parity.
func runContractColdMatrixAcceptedState(t *testing.T, f *contractJobFixture, cache **contractDeltaCache, before graph.ContractAttachment, previousCold contractColdMatrixSnapshot, files map[string]string, hooks *contractConvergenceHooks) (contractColdMatrixSnapshot, bool) {
	t.Helper()
	require.NotNil(t, *cache)
	previousCache, previousDeltas, previousLayers := *cache, (*cache).deltas, f.layers
	inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
	require.NoError(t, err)
	require.True(t, inputs.State.Accepted)
	snapshot, targets, err := NewContractFollowupCapture(f.options)(t.Context(), nil, inputs, "fixture", "")
	require.NoError(t, err)
	key := snapshot.Key
	snapshot.Release()
	beforeKey := graph.ContractAttachmentKey{RepoPrefix: before.RepoPrefix, CheckoutID: before.CheckoutID, InputVersion: before.InputVersion, InputFingerprint: before.InputFingerprint}
	ran := len(targets) != 0
	if !ran {
		require.Equal(t, beforeKey, key, "no job is valid only for the carried exact input key")
		// Physical journal debt is retained for historical readers even when
		// this exact attachment has acknowledged it. Test serving completion,
		// not mutation of historical work (including unmoved-key cutpoints).
		work, err := f.store.ContractWorkForAttachmentScopeContext(t.Context(), key, "fixture", "")
		require.NoError(t, err)
		var pending []graph.ContractWork
		for _, row := range work {
			if row.State == graph.ContractWorkPending {
				pending = append(pending, row)
			}
		}
		require.Empty(t, pending, "source-only identity carry owes no selected attachment work")
		require.Equal(t, before, contractColdMatrixAttachment(t, f), "no-op keeps the exact sealed payload")
		require.Same(t, previousCache, *cache)
		require.Equal(t, previousDeltas, (*cache).deltas, "no delta job ran")
		require.Equal(t, previousLayers, f.layers, "no fixture job or payload allocation ran")
	} else {
		require.NotEqual(t, beforeKey, key)
		require.Len(t, targets, 1)
		require.NotEmpty(t, targets[0].Work, "a key-moving edit retains its actual work obligations")
		f.layers++
		report, err := runFixtureContractJob(t, f.options, contractFixtureJob{layer: f.layers, cache: cache,
			hooks: hooks, epoch: f.epoch.Load, wait: f.waitPublished, logger: f.logger, adjust: f.adjust})
		require.NoError(t, err)
		require.True(t, report.Published)
		require.Equal(t, viewmetrics.ContractModeDelta, report.Mode)
		require.Empty(t, report.CacheReason)
		require.Equal(t, ContractFollowupChangedBoundary, report.RebuildReason)
		require.NotNil(t, *cache)
		require.Equal(t, previousDeltas+1, (*cache).deltas)
	}
	require.NotNil(t, *cache)
	require.True(t, (*cache).complete)
	require.LessOrEqual(t, (*cache).retainedBytes, contractDeltaCacheBudget)
	cold := contractColdMatrixCold(t, files)
	if !ran {
		require.Equal(t, previousCold, cold, "a no-op requires independently cold-equivalent before and after outputs")
	}
	require.Equal(t, cold, contractColdMatrixRead(t, f))
	return cold, ran
}

func runContractColdMatrixCell(t *testing.T, row contractColdMatrixFixture, path, operation string) {
	t.Helper()
	files := maps.Clone(row.files)
	if operation == "add" {
		delete(files, path)
	}
	f := newContractJobFixtureWithRegistry(t, files, contractColdMatrixRegistry())
	var cache *contractDeltaCache
	initial := runCachedContractFixture(t, f, &cache)
	require.Equal(t, viewmetrics.ContractModeFill, initial.Mode)
	require.Equal(t, "cache_missing", initial.CacheReason)
	if operation != "add" {
		contractColdMatrixPremise(t, f, row)
	}
	before := contractColdMatrixAttachment(t, f)
	previousCold := contractColdMatrixCold(t, files)
	require.Equal(t, previousCold, contractColdMatrixRead(t, f))
	if operation == "remove" {
		f.remove(path)
		delete(files, path)
	} else {
		text := row.files[path]
		if operation != "add" {
			text = contractColdMatrixEdit(row, path, operation)
		}
		f.save(path, text)
		files[path] = text
	}
	runContractColdMatrixAcceptedState(t, f, &cache, before, previousCold, files, nil)
	if operation == "add" {
		contractColdMatrixPremise(t, f, row)
	}
}

// Keep ordinary CI small. The acceptance-tagged Cartesian matrix and seeded
// campaign use the same full-payload oracle, with no elapsed-time assertions.
func TestContractDeltaColdPayloadMatrix(t *testing.T) {
	fixtures := contractColdMatrixFixtures()
	for _, cell := range []struct {
		fixture         int
		path, operation string
	}{
		{0, "server.go", "value_or_owner"},
		{0, "client.go", "remove"},
		{1, "config.go", "comment"},
		{1, "config.go", "new_function"},
		{2, "config.ts", "value_or_owner"},
		{2, "config.ts", "add"},
		{3, "Clocks.java", "value_or_owner"},
		{3, "OrderService.java", "comment"},
	} {
		row := fixtures[cell.fixture]
		t.Run(fmt.Sprintf("%s/%s/%s", row.name, cell.path, cell.operation), func(t *testing.T) {
			runContractColdMatrixCell(t, row, cell.path, cell.operation)
		})
	}
}

// A failure envelope must remove the old output, and repair must restore the
// full owners/bridges/shapes/search payload, not only its canonical contract.
func TestContractDeltaColdPayloadFailureRepair(t *testing.T) {
	row := contractColdMatrixFixtures()[0]
	files := maps.Clone(row.files)
	f := newContractJobFixtureWithRegistry(t, files, contractColdMatrixRegistry())
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	contractColdMatrixPremise(t, f, row)
	before := contractColdMatrixRead(t, f)
	for _, failing := range []bool{true, false} {
		text := row.files["server.go"]
		if failing {
			text += "\n// EXTRACT_FAIL\n"
		}
		// The real incremental door publishes its deterministic failed-source
		// envelope. It reports failure, so do not use f.save's success guard.
		contractColdMatrixSaveFailure(t, f, "server.go", text, failing)
		receipt, _, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", "fixture/server.go")
		require.NoError(t, err)
		require.Equal(t, failing, contractReceiptIsFailureEnvelope(receipt))
		report := runCachedContractFixture(t, f, &cache)
		require.Equal(t, viewmetrics.ContractModeDelta, report.Mode)
		require.Equal(t, ContractFollowupChangedBoundary, report.RebuildReason)
		files["server.go"] = text
		if failing {
			// Initial-index fixture setup refuses failed files. Build a fresh
			// independent core, apply the same failed source through its real
			// journal, then run its FIRST analysis job without P. Keep the
			// failed file and its envelope in the census, rather than treating
			// failure as deletion or borrowing the incremental actor's core.
			coldFiles := maps.Clone(files)
			delete(coldFiles, "server.go")
			cold := newContractJobFixtureWithRegistry(t, coldFiles, contractColdMatrixRegistry())
			contractColdMatrixSaveFailure(t, cold, "server.go", text, true)
			coldReport, err := cold.job(nil, nil)
			require.NoError(t, err)
			require.True(t, coldReport.Published)
			require.Equal(t, viewmetrics.ContractModeCold, coldReport.Mode)
			require.Equal(t, contractColdMatrixRead(t, cold), contractColdMatrixRead(t, f))
		} else {
			require.Equal(t, contractColdMatrixCold(t, files), contractColdMatrixRead(t, f))
		}
		if failing {
			require.NotEqual(t, before, contractColdMatrixRead(t, f))
		} else {
			require.Equal(t, before, contractColdMatrixRead(t, f))
		}
	}
}

func contractColdMatrixSaveFailure(t *testing.T, f *contractJobFixture, name, text string, failing bool) {
	t.Helper()
	path := filepath.Join(f.root, name)
	writeFile(t, path, text)
	future := time.Now().Add(time.Duration(f.epoch.Load()+1) * time.Minute)
	require.NoError(t, os.Chtimes(path, future, future))
	_, err := f.mi.IncrementalReindexRepo("fixture", []string{path})
	if !failing {
		require.NoError(t, err)
	}
	receipt, _, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", "fixture/"+name)
	require.NoError(t, err)
	require.NotNil(t, receipt)
	require.True(t, receipt.Accepted)
	require.Equal(t, failing, contractReceiptIsFailureEnvelope(receipt))
}

// Spring consumer membership changes the derived call payload even though the
// consumer file owns no provider contract of its own.
func TestContractDeltaSpringConsumerMembershipEqualsCold(t *testing.T) {
	for _, operation := range []string{"remove", "add"} {
		t.Run(operation, func(t *testing.T) {
			runContractColdMatrixCell(t, contractColdMatrixFixtures()[3], "OrderService.java", operation)
		})
	}
}

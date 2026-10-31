package languages

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// shFor returns an ExtractorPluginSpec whose command is `sh -c <script>`
// — the script consumes stdin and echoes a fixed JSON document.
func shFor(language, ext, script string) config.ExtractorPluginSpec {
	return config.ExtractorPluginSpec{
		Language:   language,
		Extensions: []string{ext},
		Command:    "sh",
		Args:       []string{"-c", script},
	}
}

func TestExtractionLimitsClampAndClassify(t *testing.T) {
	defaults := parser.DefaultExtractionLimits()
	require.Equal(t, defaults, defaults.Clamped())

	plusOne := parser.ExtractionLimits{
		MaxNodes: defaults.MaxNodes + 1, MaxEdges: defaults.MaxEdges + 1,
		MaxStdoutBytes: defaults.MaxStdoutBytes + 1, MaxResultBytes: defaults.MaxResultBytes + 1,
		MaxStderrBytes: defaults.MaxStderrBytes + 1,
	}
	require.Equal(t, defaults, plusOne.Clamped())

	maxed := parser.ExtractionLimits{
		MaxNodes: math.MaxInt, MaxEdges: math.MaxInt, MaxStdoutBytes: math.MaxInt,
		MaxResultBytes: math.MaxInt, MaxStderrBytes: math.MaxInt,
	}
	require.Equal(t, defaults, maxed.Clamped())
	require.Equal(t, parser.ExtractionLimits{}, parser.ExtractionLimits{}.Clamped(), "bounded zero must remain strict")
	require.Equal(t, parser.ExtractionLimits{}, parser.ExtractionLimits{
		MaxNodes: -1, MaxEdges: -1, MaxStdoutBytes: -1, MaxResultBytes: -1, MaxStderrBytes: -1,
	}.Clamped())

	limitErr := &parser.ExtractionLimitError{Resource: "nodes", Limit: 7, Observed: 8}
	require.ErrorIs(t, limitErr, parser.ErrExtractionLimit)
	var typed *parser.ExtractionLimitError
	require.ErrorAs(t, limitErr, &typed)
	require.Equal(t, int64(8), typed.Observed)
	var nilLimit *parser.ExtractionLimitError
	require.Equal(t, parser.ErrExtractionLimit.Error(), nilLimit.Error())
	assert.True(t, errors.Is(limitErr, parser.ErrExtractionLimit))
}

func TestSubprocessExtractor_EmitsNodesAndEdges(t *testing.T) {
	// One function node + one EdgeDefines from the file to it. The
	// script drains stdin so the parent's stdin pipe always closes.
	const json = `{"nodes":[` +
		`{"id":"foo.ml::main","kind":"function","name":"main","start_line":3,"end_line":9,"meta":{"k":"v"}}` +
		`],"edges":[` +
		`{"from":"foo.ml","to":"foo.ml::main","kind":"defines","line":3}` +
		`]}`
	ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; printf '%s' '"+json+"'"), nil)

	res, err := ext.Extract("foo.ml", []byte("let\nx\nmain () = ()\n"))
	require.NoError(t, err)
	require.NotNil(t, res)

	// File node is always emitted first.
	require.GreaterOrEqual(t, len(res.Nodes), 2)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
	assert.Equal(t, "foo.ml", res.Nodes[0].ID)
	assert.Equal(t, "ml", res.Nodes[0].Language)

	fn := res.Nodes[1]
	assert.Equal(t, "foo.ml::main", fn.ID)
	assert.Equal(t, graph.KindFunction, fn.Kind)
	assert.Equal(t, "main", fn.Name)
	assert.Equal(t, 3, fn.StartLine)
	assert.Equal(t, 9, fn.EndLine)
	assert.Equal(t, "ml", fn.Language) // defaulted from extractor language
	assert.Equal(t, "v", fn.Meta["k"])

	require.Len(t, res.Edges, 1)
	assert.Equal(t, "foo.ml", res.Edges[0].From)
	assert.Equal(t, "foo.ml::main", res.Edges[0].To)
	assert.Equal(t, graph.EdgeDefines, res.Edges[0].Kind)
}

func TestSubprocessExtractor_SynthesizesMissingID(t *testing.T) {
	const json = `{"nodes":[{"kind":"type","name":"Widget"}]}`
	ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; printf '%s' '"+json+"'"), nil)

	res, err := ext.Extract("w.ml", []byte("x"))
	require.NoError(t, err)
	require.Len(t, res.Nodes, 2)
	assert.Equal(t, "w.ml::Widget", res.Nodes[1].ID) // filePath + "::" + name
}

func TestSubprocessExtractor_DropsInvalidKind(t *testing.T) {
	const json = `{"nodes":[` +
		`{"id":"bad","kind":"not_a_real_kind","name":"x"},` +
		`{"id":"good","kind":"function","name":"y"}` +
		`]}`
	ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; printf '%s' '"+json+"'"), nil)

	res, err := ext.Extract("z.ml", []byte("x"))
	require.NoError(t, err)
	// file node + the one valid node; the invalid-kind node is dropped.
	require.Len(t, res.Nodes, 2)
	assert.Equal(t, "good", res.Nodes[1].ID)
}

func TestSubprocessExtractor_FailingCommandDegradesToFileNode(t *testing.T) {
	ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; exit 3"), nil)

	res, err := ext.Extract("e.ml", []byte("x\ny\n"))
	require.NoError(t, err) // never fails the index
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
	assert.Empty(t, res.Edges)
}

func TestSubprocessExtractor_BadJSONDegradesToFileNode(t *testing.T) {
	ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; printf 'not json'"), nil)

	res, err := ext.Extract("b.ml", []byte("x"))
	require.NoError(t, err)
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
}

func decodePluginForTest(document string, limits parser.ExtractionLimits) (pluginDocument, error) {
	limits = limits.Clamped()
	budget, err := newPluginResultBudget(int64(limits.MaxResultBytes), int64(len(document)))
	if err != nil {
		return pluginDocument{}, err
	}
	return decodePluginDocument(context.Background(), strings.NewReader(document), "x", limits, budget)
}

func requirePluginLimit(t *testing.T, err error, resource string, limit, observed int64) {
	t.Helper()
	require.ErrorIs(t, err, parser.ErrExtractionLimit)
	var limitErr *parser.ExtractionLimitError
	require.ErrorAs(t, err, &limitErr)
	assert.Equal(t, resource, limitErr.Resource)
	assert.Equal(t, limit, limitErr.Limit)
	assert.Equal(t, observed, limitErr.Observed)
}

func TestDecodePluginDocument_CompatibilityAndCumulativeCaps(t *testing.T) {
	t.Run("top-level null", func(t *testing.T) {
		document, err := decodePluginForTest("null", parser.DefaultExtractionLimits())
		require.NoError(t, err)
		assert.Empty(t, document.Nodes)
		assert.Empty(t, document.Edges)
	})

	t.Run("case-insensitive duplicate fields are last-wins", func(t *testing.T) {
		limits := parser.DefaultExtractionLimits()
		limits.MaxNodes = 3
		document, err := decodePluginForTest(`{"Nodes":[{"id":"a","kind":"function"}],"nOdEs":[{"id":"b","kind":"function"}]}`, limits)
		require.NoError(t, err)
		require.Len(t, document.Nodes, 1)
		assert.Equal(t, "b", document.Nodes[0].ID)
	})

	t.Run("duplicate fields count cumulatively", func(t *testing.T) {
		limits := parser.DefaultExtractionLimits()
		limits.MaxNodes = 3
		_, err := decodePluginForTest(`{"nodes":[{"id":"a","kind":"function"}],"NODES":[{"id":"b","kind":"function"},{"id":"c","kind":"function"}]}`, limits)
		requirePluginLimit(t, err, "nodes", 3, 4)
	})

	t.Run("invalid raw nodes still consume the cap", func(t *testing.T) {
		limits := parser.DefaultExtractionLimits()
		limits.MaxNodes = 2
		_, err := decodePluginForTest(`{"nodes":[{"kind":"invalid"},{"id":"b","kind":"function"}]}`, limits)
		requirePluginLimit(t, err, "nodes", 2, 3)
	})

	t.Run("raw edges enforce exact plus one", func(t *testing.T) {
		limits := parser.DefaultExtractionLimits()
		limits.MaxEdges = 1
		_, err := decodePluginForTest(`{"edges":[{"from":"a","to":"b","kind":"calls"},{"from":"b","to":"c","kind":"calls"}]}`, limits)
		requirePluginLimit(t, err, "edges", 1, 2)
	})
}

func TestDecodePluginDocument_WireAndSynthesizedIDBounds(t *testing.T) {
	t.Run("stdout exact and plus one", func(t *testing.T) {
		limits := parser.DefaultExtractionLimits()
		limits.MaxStdoutBytes = len("null")
		_, err := decodePluginForTest("null", limits)
		require.NoError(t, err)
		_, err = decodePluginForTest("null ", limits)
		requirePluginLimit(t, err, "stdout_bytes", int64(len("null")), int64(len("null")+1))
	})

	t.Run("combined result starts with stdout", func(t *testing.T) {
		limits := parser.DefaultExtractionLimits()
		limits.MaxResultBytes = len("null")
		_, err := decodePluginForTest("null", limits)
		require.NoError(t, err)
		limits.MaxResultBytes--
		_, err = decodePluginForTest("null", limits)
		requirePluginLimit(t, err, "result_bytes", int64(len("null")-1), int64(len("null")))
	})

	t.Run("synthesized id exact and plus one", func(t *testing.T) {
		const wire = `{"nodes":[{"kind":"function","name":"abc"}]}`
		const synthesized = "x::abc"
		limits := parser.DefaultExtractionLimits()
		limits.MaxNodes = 2
		limits.MaxResultBytes = len(wire) + len(synthesized)
		document, err := decodePluginForTest(wire, limits)
		require.NoError(t, err)
		require.Len(t, document.Nodes, 1)

		limits.MaxResultBytes--
		_, err = decodePluginForTest(wire, limits)
		requirePluginLimit(t, err, "result_bytes", int64(len(wire)+len(synthesized)-1), int64(len(wire)+len(synthesized)))
	})

	t.Run("overwritten synthesized ids are not refunded", func(t *testing.T) {
		const wire = `{"nodes":[{"kind":"function","name":"abc"}],"nodes":[{"kind":"function","name":"abc"}]}`
		const synthesized = "x::abc"
		limits := parser.DefaultExtractionLimits()
		limits.MaxNodes = 3
		limits.MaxResultBytes = len(wire) + len(synthesized)
		_, err := decodePluginForTest(wire, limits)
		requirePluginLimit(t, err, "result_bytes", int64(len(wire)+len(synthesized)), int64(len(wire)+len(synthesized)+1))
	})
}

func TestSubprocessExtractor_BoundedExactAndPlusOne(t *testing.T) {
	run := func(wire string, limits parser.ExtractionLimits) (*parser.ExtractionResult, parser.ExtractionUsage, error) {
		ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; printf '%s' '"+wire+"'"), nil)
		return ext.ExtractBounded(context.Background(), "x.ml", nil, limits)
	}

	t.Run("nodes", func(t *testing.T) {
		const wire = `{"nodes":[{"id":"a","kind":"function"}]}`
		limits := parser.DefaultExtractionLimits()
		limits.MaxNodes = 2
		result, usage, err := run(wire, limits)
		require.NoError(t, err)
		require.Len(t, result.Nodes, 2)
		assert.Equal(t, parser.ExtractionUsage{
			RawNodes: 2, StdoutBytes: int64(len(wire)), ResultBytes: int64(len(wire)),
		}, usage)

		limits.MaxNodes = 1
		result, usage, err = run(wire, limits)
		assert.Nil(t, result)
		assert.Empty(t, usage)
		requirePluginLimit(t, err, "nodes", 1, 2)
	})

	t.Run("edges including strict zero", func(t *testing.T) {
		const emptyWire = `{"edges":[]}`
		limits := parser.DefaultExtractionLimits()
		limits.MaxEdges = 0
		result, usage, err := run(emptyWire, limits)
		require.NoError(t, err)
		require.Len(t, result.Nodes, 1)
		assert.Equal(t, parser.ExtractionUsage{
			RawNodes: 1, StdoutBytes: int64(len(emptyWire)), ResultBytes: int64(len(emptyWire)),
		}, usage)

		const wire = `{"edges":[{"from":"a","to":"b","kind":"calls"}]}`
		limits.MaxEdges = 1
		result, usage, err = run(wire, limits)
		require.NoError(t, err)
		require.Len(t, result.Edges, 1)
		assert.Equal(t, parser.ExtractionUsage{
			RawNodes: 1, RawEdges: 1, StdoutBytes: int64(len(wire)), ResultBytes: int64(len(wire)),
		}, usage)

		limits.MaxEdges = 0
		result, usage, err = run(wire, limits)
		assert.Nil(t, result)
		assert.Empty(t, usage)
		requirePluginLimit(t, err, "edges", 0, 1)
	})

	t.Run("stdout bytes", func(t *testing.T) {
		const wire = "null"
		limits := parser.DefaultExtractionLimits()
		limits.MaxStdoutBytes = len(wire)
		result, usage, err := run(wire, limits)
		require.NoError(t, err)
		require.Len(t, result.Nodes, 1)
		assert.Equal(t, parser.ExtractionUsage{RawNodes: 1, StdoutBytes: 4, ResultBytes: 4}, usage)

		limits.MaxStdoutBytes--
		result, usage, err = run(wire, limits)
		assert.Nil(t, result)
		assert.Empty(t, usage)
		requirePluginLimit(t, err, "stdout_bytes", int64(len(wire)-1), int64(len(wire)))
	})

	t.Run("combined result bytes", func(t *testing.T) {
		const wire = `{"nodes":[{"kind":"function","name":"n"}]}`
		const synthesized = "x.ml::n"
		limits := parser.DefaultExtractionLimits()
		limits.MaxNodes = 2
		limits.MaxResultBytes = len(wire) + len(synthesized)
		result, usage, err := run(wire, limits)
		require.NoError(t, err)
		require.Len(t, result.Nodes, 2)
		assert.Equal(t, parser.ExtractionUsage{
			RawNodes: 2, StdoutBytes: int64(len(wire)), ResultBytes: int64(len(wire) + len(synthesized)),
		}, usage)

		limits.MaxResultBytes--
		result, usage, err = run(wire, limits)
		assert.Nil(t, result)
		assert.Empty(t, usage)
		requirePluginLimit(t, err, "result_bytes", int64(len(wire)+len(synthesized)-1), int64(len(wire)+len(synthesized)))
	})
}

func TestSubprocessExtractor_ReportsConservativeUsage(t *testing.T) {
	limits := parser.DefaultExtractionLimits()

	t.Run("null charges only exact stdout and file node", func(t *testing.T) {
		ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; printf 'null'"), nil)
		result, usage, err := ext.ExtractBounded(context.Background(), "x.ml", nil, limits)
		require.NoError(t, err)
		require.Len(t, result.Nodes, 1)
		assert.Equal(t, parser.ExtractionUsage{RawNodes: 1, StdoutBytes: 4, ResultBytes: 4}, usage)
	})

	t.Run("duplicate and invalid rows remain charged", func(t *testing.T) {
		const wire = `{"nodes":[{"id":"ignored","kind":"invalid"},{"id":"old","kind":"function"}],"NODES":[{"kind":"function","name":"last"}],"edges":[{"from":"","to":"x","kind":"calls"}],"EDGES":[{"from":"a","to":"b","kind":"calls"}]}`
		ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; printf '%s' '"+wire+"'"), nil)
		result, usage, err := ext.ExtractBounded(context.Background(), "x.ml", nil, limits)
		require.NoError(t, err)
		require.Len(t, result.Nodes, 2)
		require.Len(t, result.Edges, 1)
		assert.Equal(t, parser.ExtractionUsage{
			RawNodes: 4, RawEdges: 2, StdoutBytes: int64(len(wire)),
			ResultBytes: int64(len(wire) + len("x.ml::last")),
		}, usage)
	})
}

func TestSubprocessExtractor_BoundedProcessAndFailureSemantics(t *testing.T) {
	limits := parser.DefaultExtractionLimits()

	t.Run("zero node budget rejects before command", func(t *testing.T) {
		ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; printf 'null'"), nil)
		zero := limits
		zero.MaxNodes = 0
		result, usage, err := ext.ExtractBounded(context.Background(), "x.ml", nil, zero)
		assert.Nil(t, result)
		assert.Empty(t, usage)
		requirePluginLimit(t, err, "nodes", 0, 1)
	})

	t.Run("bounded operational errors have no partial result", func(t *testing.T) {
		for name, script := range map[string]string{
			"nonzero":   "cat >/dev/null; printf 'null'; exit 3",
			"malformed": "cat >/dev/null; printf 'not-json'",
		} {
			t.Run(name, func(t *testing.T) {
				ext := NewSubprocessExtractor(shFor("ml", ".ml", script), nil)
				result, usage, err := ext.ExtractBounded(context.Background(), "x.ml", nil, limits)
				assert.Nil(t, result)
				assert.Empty(t, usage)
				require.Error(t, err)
			})
		}
	})

	t.Run("stderr cap is retention only", func(t *testing.T) {
		ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; printf 'abcdefghijklmnopqrstuvwxyz' >&2; printf 'null'"), nil)
		for _, retained := range []int{0, 8} {
			bounded := limits
			bounded.MaxStderrBytes = retained
			result, usage, err := ext.ExtractBounded(context.Background(), "x.ml", nil, bounded)
			require.NoError(t, err)
			require.Len(t, result.Nodes, 1)
			assert.Equal(t, parser.ExtractionUsage{RawNodes: 1, StdoutBytes: 4, ResultBytes: 4}, usage)
		}
	})

	t.Run("caller cancellation is prompt and exact", func(t *testing.T) {
		ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; exec sleep 10"), nil)
		ctx, cancel := context.WithCancel(context.Background())
		timer := time.AfterFunc(25*time.Millisecond, cancel)
		defer timer.Stop()
		started := time.Now()
		result, usage, err := ext.ExtractBounded(ctx, "x.ml", nil, limits)
		assert.Nil(t, result)
		assert.Empty(t, usage)
		require.ErrorIs(t, err, context.Canceled)
		assert.Less(t, time.Since(started), 2*time.Second)
	})

	t.Run("caller deadline is prompt and exact", func(t *testing.T) {
		ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; exec sleep 10"), nil)
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		started := time.Now()
		result, usage, err := ext.ExtractBounded(ctx, "x.ml", nil, limits)
		assert.Nil(t, result)
		assert.Empty(t, usage)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Less(t, time.Since(started), 2*time.Second)
	})

	t.Run("descendant-held stdout is bounded by WaitDelay", func(t *testing.T) {
		ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; (sleep 1) & printf 'null'"), nil)
		started := time.Now()
		result, usage, err := ext.ExtractBounded(context.Background(), "x.ml", nil, limits)
		assert.Nil(t, result)
		assert.Empty(t, usage)
		require.Error(t, err)
		assert.Less(t, time.Since(started), 2*time.Second)
	})

	t.Run("stdout overflow cancels a sleeping plugin", func(t *testing.T) {
		ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; printf '0123456789'; exec sleep 10"), nil)
		bounded := limits
		bounded.MaxStdoutBytes = 4
		started := time.Now()
		result, usage, err := ext.ExtractBounded(context.Background(), "x.ml", nil, bounded)
		assert.Nil(t, result)
		assert.Empty(t, usage)
		requirePluginLimit(t, err, "stdout_bytes", 4, 5)
		assert.Less(t, time.Since(started), 2*time.Second)
	})

	t.Run("internal timeout is ordinary", func(t *testing.T) {
		spec := shFor("ml", ".ml", "cat >/dev/null; exec sleep 10")
		spec.TimeoutMs = 25
		ext := NewSubprocessExtractor(spec, nil)
		result, usage, err := ext.ExtractBounded(context.Background(), "x.ml", nil, limits)
		assert.Nil(t, result)
		assert.Empty(t, usage)
		require.Error(t, err)
		assert.NotErrorIs(t, err, context.DeadlineExceeded)
		assert.NotErrorIs(t, err, context.Canceled)
	})

	t.Run("legacy stdout overflow degrades to file only", func(t *testing.T) {
		ext := NewSubprocessExtractor(shFor("ml", ".ml", "exec dd if=/dev/zero bs=1048576 count=17 2>/dev/null"), nil)
		result, err := ext.Extract("x.ml", nil)
		require.NoError(t, err)
		require.Len(t, result.Nodes, 1)
		assert.Empty(t, result.Edges)
	})
}

func TestRegisterExtractorPlugins_SkipsInvalidAndCollisions(t *testing.T) {
	reg := parser.NewRegistry()
	reg.Register(NewGoExtractor()) // claims "go" and ".go"

	RegisterExtractorPlugins(reg, []config.ExtractorPluginSpec{
		{Language: "", Extensions: []string{".x"}, Command: "cat"},        // no language
		{Language: "x", Extensions: nil, Command: "cat"},                  // no extensions
		{Language: "x", Extensions: []string{".x"}, Command: ""},          // no command
		{Language: "go", Extensions: []string{".golang"}, Command: "cat"}, // language collision
		{Language: "mygo", Extensions: []string{".go"}, Command: "cat"},   // extension collision
		{Language: "ml", Extensions: []string{".ml"}, Command: "cat"},     // valid
	}, nil)

	_, hasML := reg.GetByLanguage("ml")
	assert.True(t, hasML)
	_, hasMygo := reg.GetByLanguage("mygo")
	assert.False(t, hasMygo)
	// The built-in Go extractor is untouched.
	e, ok := reg.GetByLanguage("go")
	require.True(t, ok)
	assert.Equal(t, "go", e.Language())
}

// TestStripSecretEnv verifies the plugin subprocess environment drops
// secret-shaped entries (provider API keys, tokens, AWS secrets, bearer
// vars) while keeping PATH and other non-secret vars — so an extractor
// command can't read and exfiltrate them via os.Environ().
func TestStripSecretEnv(t *testing.T) {
	in := []string{
		"PATH=/usr/local/bin:/usr/bin",
		"OPENAI_API_KEY=sk-abc",
		"GOOGLE_API_KEY=AIza",
		"API_KEY=barekey",
		"GOOGLEAPIKEY=concatkey",
		"GORTEX_LLM_API_KEY=glp-xyz",
		"GORTEX_EMBEDDINGS_API_KEY=e-123",
		"GITHUB_TOKEN=ghp_123",
		"AUTH_TOKEN=tok",
		"AWS_SECRET_ACCESS_KEY=wJalr",
		"MY_SECRET=xyz",
		"GITHUB_BEARER=b",
		"HDR_AUTHORIZATION=Bearer abc",
		"APIKEY=plainkey",
		"MY_SEGMENTER=split", // not secret-shaped
		"GORTEX_FILE_PATH=repo/a.go",
		"GITHUB_ACTIONS", // no '=', kept
	}
	got := map[string]bool{}
	for _, kv := range stripSecretEnv(in) {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		got[key] = true
	}
	for _, keep := range []string{"PATH", "MY_SEGMENTER", "GORTEX_FILE_PATH", "GITHUB_ACTIONS"} {
		assert.True(t, got[keep], "expected %q to be kept", keep)
	}
	dropped := []string{
		"OPENAI_API_KEY", "GOOGLE_API_KEY", "API_KEY", "GOOGLEAPIKEY",
		"GORTEX_LLM_API_KEY", "GORTEX_EMBEDDINGS_API_KEY",
		"GITHUB_TOKEN", "AUTH_TOKEN", "AWS_SECRET_ACCESS_KEY", "MY_SECRET",
		"GITHUB_BEARER", "HDR_AUTHORIZATION", "APIKEY",
	}
	for _, name := range dropped {
		assert.False(t, got[name], "expected %q to be dropped", name)
	}
}

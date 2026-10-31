package languages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/platform"
)

// defaultExtractorPluginTimeout bounds a plugin subprocess when the
// spec doesn't set TimeoutMs. Generous enough for a real extractor pass
// over a single file, small enough that a hung plugin never stalls the
// whole index.
const (
	defaultExtractorPluginTimeout = 5000 * time.Millisecond
	extractorPluginWaitDelay      = 250 * time.Millisecond
)

// pluginNode mirrors one entry of the plugin's JSON `nodes` array. The
// field names are the documented wire shape; meta is free-form.
type pluginNode struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Name      string         `json:"name"`
	FilePath  string         `json:"file_path"`
	StartLine int            `json:"start_line"`
	EndLine   int            `json:"end_line"`
	Language  string         `json:"language"`
	Meta      map[string]any `json:"meta"`
}

// pluginEdge mirrors one entry of the plugin's JSON `edges` array.
type pluginEdge struct {
	From     string         `json:"from"`
	To       string         `json:"to"`
	Kind     string         `json:"kind"`
	FilePath string         `json:"file_path"`
	Line     int            `json:"line"`
	Meta     map[string]any `json:"meta"`
}

// pluginDocument is the full JSON document a plugin writes to stdout.
type pluginDocument struct {
	Nodes    []pluginNode `json:"nodes"`
	Edges    []pluginEdge `json:"edges"`
	rawNodes int
	rawEdges int
}

const pluginCancellationPollMask = 127

type cappedPluginBuffer struct {
	buffer       bytes.Buffer
	limit        int
	truncated    bool
	onOverflow   func()
	overflowOnce sync.Once
}

func (b *cappedPluginBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if remaining > original {
			remaining = original
		}
		_, _ = b.buffer.Write(p[:remaining])
	}
	if remaining < original {
		b.truncated = true
		b.overflowOnce.Do(func() {
			if b.onOverflow != nil {
				b.onOverflow()
			}
		})
	}
	return original, nil
}

func (b *cappedPluginBuffer) String() string { return b.buffer.String() }

type pluginResultBudget struct {
	limit    int64
	observed int64
}

func newPluginResultBudget(limit, stdoutBytes int64) (*pluginResultBudget, error) {
	budget := &pluginResultBudget{limit: limit}
	if stdoutBytes < 0 || stdoutBytes > limit {
		return nil, &parser.ExtractionLimitError{
			Resource: "result_bytes",
			Limit:    limit,
			Observed: limit + 1,
		}
	}
	budget.observed = stdoutBytes
	return budget, nil
}

func (b *pluginResultBudget) reserveSynthesizedID(filePath, name string) error {
	if b == nil {
		return nil
	}
	if err := b.reserveSynthesizedBytes(int64(len(filePath))); err != nil {
		return err
	}
	if err := b.reserveSynthesizedBytes(2); err != nil {
		return err
	}
	return b.reserveSynthesizedBytes(int64(len(name)))
}

func (b *pluginResultBudget) reserveSynthesizedBytes(size int64) error {
	if size < 0 || b.observed > b.limit || size > b.limit-b.observed {
		return &parser.ExtractionLimitError{
			Resource: "result_bytes",
			Limit:    b.limit,
			Observed: b.limit + 1,
		}
	}
	b.observed += size
	return nil
}

func pollPluginContext(ctx context.Context, position int) error {
	if position&pluginCancellationPollMask != 0 {
		return nil
	}
	return ctx.Err()
}

func decodePluginDocument(ctx context.Context, reader io.Reader, filePath string, limits parser.ExtractionLimits, budget *pluginResultBudget) (pluginDocument, error) {
	if err := ctx.Err(); err != nil {
		return pluginDocument{}, err
	}
	readLimit := int64(limits.MaxStdoutBytes) + 1
	limited := &io.LimitedReader{R: reader, N: readLimit}
	decoder := json.NewDecoder(limited)
	document, decodeErr := decodePluginObject(ctx, decoder, filePath, limits, budget)
	if decodeErr != nil {
		if err := ctx.Err(); err != nil {
			return pluginDocument{}, err
		}
		observed := readLimit - limited.N
		if observed > int64(limits.MaxStdoutBytes) {
			return pluginDocument{}, stdoutLimitError(limits.MaxStdoutBytes)
		}
		return pluginDocument{}, decodeErr
	}

	token, trailingErr := decoder.Token()
	observed := readLimit - limited.N
	if observed > int64(limits.MaxStdoutBytes) {
		return pluginDocument{}, stdoutLimitError(limits.MaxStdoutBytes)
	}
	if err := ctx.Err(); err != nil {
		return pluginDocument{}, err
	}
	if trailingErr != io.EOF {
		if trailingErr != nil {
			return pluginDocument{}, trailingErr
		}
		return pluginDocument{}, fmt.Errorf("unexpected trailing JSON token %v", token)
	}
	return document, nil
}

func stdoutLimitError(limit int) error {
	return &parser.ExtractionLimitError{
		Resource: "stdout_bytes",
		Limit:    int64(limit),
		Observed: int64(limit) + 1,
	}
}

func decodePluginObject(ctx context.Context, decoder *json.Decoder, filePath string, limits parser.ExtractionLimits, budget *pluginResultBudget) (pluginDocument, error) {
	opening, err := decoder.Token()
	if err != nil {
		return pluginDocument{}, err
	}
	if opening == nil {
		return pluginDocument{}, nil
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return pluginDocument{}, fmt.Errorf("plugin output must be a JSON object or null")
	}

	var document pluginDocument
	rawNodes, rawEdges, fields := 0, 0, 0
	for decoder.More() {
		if err := pollPluginContext(ctx, fields); err != nil {
			return pluginDocument{}, err
		}
		fields++
		keyToken, err := decoder.Token()
		if err != nil {
			return pluginDocument{}, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return pluginDocument{}, fmt.Errorf("plugin output contains a non-string field name")
		}
		switch {
		case strings.EqualFold(key, "nodes"):
			document.Nodes, err = decodePluginNodes(ctx, decoder, filePath, limits.MaxNodes, &rawNodes, budget)
		case strings.EqualFold(key, "edges"):
			document.Edges, err = decodePluginEdges(ctx, decoder, limits.MaxEdges, &rawEdges)
		default:
			err = skipPluginJSONValue(ctx, decoder)
		}
		if err != nil {
			return pluginDocument{}, err
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return pluginDocument{}, err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return pluginDocument{}, fmt.Errorf("plugin output object is not closed")
	}
	document.rawNodes = rawNodes
	document.rawEdges = rawEdges
	return document, ctx.Err()
}

func decodePluginNodes(ctx context.Context, decoder *json.Decoder, filePath string, maxTotal int, rawCount *int, budget *pluginResultBudget) ([]pluginNode, error) {
	opening, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if opening == nil {
		return nil, nil
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '[' {
		return nil, fmt.Errorf("plugin nodes must be an array or null")
	}

	nodes := make([]pluginNode, 0)
	for decoder.More() {
		if err := pollPluginContext(ctx, *rawCount); err != nil {
			return nil, err
		}
		if *rawCount >= maxTotal-1 {
			return nil, &parser.ExtractionLimitError{
				Resource: "nodes",
				Limit:    int64(maxTotal),
				Observed: int64(*rawCount) + 2,
			}
		}
		(*rawCount)++
		var node pluginNode
		if err := decoder.Decode(&node); err != nil {
			return nil, err
		}
		if graph.ValidNodeKind(graph.NodeKind(strings.TrimSpace(node.Kind))) && strings.TrimSpace(node.ID) == "" {
			fp := node.FilePath
			if fp == "" {
				fp = filePath
			}
			if err := budget.reserveSynthesizedID(fp, node.Name); err != nil {
				return nil, err
			}
		}
		nodes = append(nodes, node)
	}
	closing, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != ']' {
		return nil, fmt.Errorf("plugin nodes array is not closed")
	}
	return nodes, ctx.Err()
}

func decodePluginEdges(ctx context.Context, decoder *json.Decoder, maxTotal int, rawCount *int) ([]pluginEdge, error) {
	opening, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if opening == nil {
		return nil, nil
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '[' {
		return nil, fmt.Errorf("plugin edges must be an array or null")
	}

	edges := make([]pluginEdge, 0)
	for decoder.More() {
		if err := pollPluginContext(ctx, *rawCount); err != nil {
			return nil, err
		}
		if *rawCount >= maxTotal {
			return nil, &parser.ExtractionLimitError{
				Resource: "edges",
				Limit:    int64(maxTotal),
				Observed: int64(*rawCount) + 1,
			}
		}
		(*rawCount)++
		var edge pluginEdge
		if err := decoder.Decode(&edge); err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	closing, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != ']' {
		return nil, fmt.Errorf("plugin edges array is not closed")
	}
	return edges, ctx.Err()
}

func skipPluginJSONValue(ctx context.Context, decoder *json.Decoder) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' && delimiter != '[' {
		return nil
	}
	depth, tokens := 1, 0
	for depth > 0 {
		if err := pollPluginContext(ctx, tokens); err != nil {
			return err
		}
		tokens++
		token, err = decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok = token.(json.Delim)
		if !ok {
			continue
		}
		switch delimiter {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}
	return ctx.Err()
}

// SubprocessExtractor is a parser.Extractor backed by an external
// command — the "register a custom extractor pass without writing Go"
// entry point. Extract pipes the file content to the command (with
// GORTEX_FILE_PATH in the env), reads a JSON node/edge document from
// stdout, and folds it into the graph. Any failure (non-zero exit, bad
// JSON, timeout) degrades to just the file node so a misbehaving plugin
// never fails the whole index.
type SubprocessExtractor struct {
	language string
	exts     []string
	command  string
	args     []string
	timeout  time.Duration
	log      *zap.Logger
	dropOnce sync.Once
}

// NewSubprocessExtractor builds a SubprocessExtractor from a spec. The
// extensions are normalised (leading dot enforced); a zero TimeoutMs
// uses defaultExtractorPluginTimeout.
func NewSubprocessExtractor(spec config.ExtractorPluginSpec, log *zap.Logger) *SubprocessExtractor {
	if log == nil {
		log = zap.NewNop()
	}
	timeout := defaultExtractorPluginTimeout
	if spec.TimeoutMs > 0 {
		timeout = time.Duration(spec.TimeoutMs) * time.Millisecond
	}
	return &SubprocessExtractor{
		language: strings.TrimSpace(spec.Language),
		exts:     normalizeGrammarExtensions(spec.Extensions),
		command:  strings.TrimSpace(spec.Command),
		args:     append([]string(nil), spec.Args...),
		timeout:  timeout,
		log:      log,
	}
}

func (e *SubprocessExtractor) Language() string     { return e.language }
func (e *SubprocessExtractor) Extensions() []string { return e.exts }

// fileNode builds the always-emitted KindFile node for a path. Its ID
// is the file path, matching every built-in extractor's convention so
// EdgeDefines from plugins resolve against the same file node.
func (e *SubprocessExtractor) fileNode(filePath string, src []byte) *graph.Node {
	return &graph.Node{
		ID:        filePath,
		Kind:      graph.KindFile,
		Name:      filePath,
		FilePath:  filePath,
		StartLine: 1,
		EndLine:   bytes.Count(src, []byte("\n")) + 1,
		Language:  e.language,
	}
}

// Extract preserves the legacy extractor contract: plugin failures, including
// bounded-output failures, degrade to a file-only result and never fail indexing.
func (e *SubprocessExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	result, _, err := e.ExtractBounded(context.Background(), filePath, src, parser.DefaultExtractionLimits())
	if err == nil {
		return result, nil
	}
	e.log.Warn("extractor plugin: bounded extraction failed — degraded to file node only",
		zap.String("language", e.language), zap.String("file", filePath), zap.Error(err))
	return e.fileOnlyResult(filePath, src), nil
}

// shellUnsafePathChars lists shell metacharacters that must not appear in
// GORTEX_FILE_PATH. A repo-controlled path that reaches a plugin which
// expands the variable through a shell could otherwise inject
// commands/operators or split the argument. The backtick is written as a
// raw-string literal; the `"` and `\` are escaped.
const shellUnsafePathChars = " ;|&<>()\"'\\$" + "`="

// ExtractBounded runs the configured plugin with hard wire and result limits.
// It is an all-or-nothing capability: caller cancellation, limit violations,
// command failures, timeouts and malformed output return no partial result.
// The legacy Extract wrapper alone converts those errors to a file-only result.
func (e *SubprocessExtractor) ExtractBounded(ctx context.Context, filePath string, src []byte, requested parser.ExtractionLimits) (*parser.ExtractionResult, parser.ExtractionUsage, error) {
	var noUsage parser.ExtractionUsage
	if ctx == nil {
		ctx = context.Background()
	}
	limits := requested.Clamped()
	if err := ctx.Err(); err != nil {
		return nil, noUsage, err
	}
	if limits.MaxNodes < 1 {
		return nil, noUsage, &parser.ExtractionLimitError{Resource: "nodes", Limit: 0, Observed: 1}
	}
	// filePath is a repo-relative name the repository controls, so it can
	// carry bytes that break env-var/shell handling when a plugin later
	// expands $GORTEX_FILE_PATH. Reject control characters and shell
	// metacharacters up front rather than exporting a value that, in a
	// shell-expanding plugin, could inject commands (`;`), expand
	// variables/commands (`$`, backticks), splice env vars (`=`), or
	// word-split the path (space). `\\` and `` ` `` in the second set are
	// the backslash and backtick; `\\$` is a literal dollar before the
	// backtick (see the shellUnsafeChars const).
	if strings.ContainsAny(filePath, "\x00\n\r\t\v\f") || strings.ContainsAny(filePath, shellUnsafePathChars) {
		return nil, noUsage, fmt.Errorf("extractor plugin: file path %q contains a shell-unsafe character", filePath)
	}

	result := e.fileOnlyResult(filePath, src)
	if e.command == "" {
		return result, parser.ExtractionUsage{RawNodes: 1}, nil
	}

	runCtx, runCancel := context.WithTimeout(ctx, e.timeout)
	defer runCancel()

	stdout := &cappedPluginBuffer{limit: limits.MaxStdoutBytes, onOverflow: runCancel}
	stderr := &cappedPluginBuffer{limit: limits.MaxStderrBytes}
	// command/args are operator-declared config, not user-derived input.
	cmd := exec.CommandContext(runCtx, e.command, e.args...) //nolint:gosec
	platform.ConfigureBackgroundCommand(cmd)
	cmd.WaitDelay = extractorPluginWaitDelay
	cmd.Stdin = bytes.NewReader(src)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(), "GORTEX_FILE_PATH="+filePath)

	runErr := cmd.Run()
	if err := ctx.Err(); err != nil {
		return nil, noUsage, err
	}
	if stdout.truncated {
		return nil, noUsage, stdoutLimitError(limits.MaxStdoutBytes)
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return nil, noUsage, fmt.Errorf("extractor plugin timed out after %s", e.timeout)
	}
	if runErr != nil {
		return nil, noUsage, fmt.Errorf("extractor plugin failed (stderr=%q, truncated=%t): %w",
			strings.TrimSpace(stderr.String()), stderr.truncated, runErr)
	}
	if stderr.truncated {
		e.log.Warn("extractor plugin: stderr truncated",
			zap.String("language", e.language), zap.String("file", filePath),
			zap.Int("retained_bytes", limits.MaxStderrBytes))
	}

	stdoutBytes := int64(stdout.buffer.Len())
	budget, budgetErr := newPluginResultBudget(int64(limits.MaxResultBytes), stdoutBytes)
	if budgetErr != nil {
		return nil, noUsage, budgetErr
	}
	document, decodeErr := decodePluginDocument(ctx, bytes.NewReader(stdout.buffer.Bytes()), filePath, limits, budget)
	if err := ctx.Err(); err != nil {
		return nil, noUsage, err
	}
	if decodeErr != nil {
		if errors.Is(decodeErr, parser.ErrExtractionLimit) {
			return nil, noUsage, decodeErr
		}
		return nil, noUsage, fmt.Errorf("decode extractor plugin output: %w", decodeErr)
	}
	converted, err := e.convertPluginDocument(ctx, filePath, result, document)
	if err != nil {
		return nil, noUsage, err
	}
	usage := parser.ExtractionUsage{
		RawNodes:    document.rawNodes + 1,
		RawEdges:    document.rawEdges,
		StdoutBytes: stdoutBytes,
		ResultBytes: budget.observed,
	}
	return converted, usage, nil
}

func (e *SubprocessExtractor) fileOnlyResult(filePath string, src []byte) *parser.ExtractionResult {
	return &parser.ExtractionResult{Nodes: []*graph.Node{e.fileNode(filePath, src)}}
}

func (e *SubprocessExtractor) convertPluginDocument(ctx context.Context, filePath string, fileOnly *parser.ExtractionResult, document pluginDocument) (*parser.ExtractionResult, error) {
	result := &parser.ExtractionResult{
		Nodes: make([]*graph.Node, 0, len(fileOnly.Nodes)+len(document.Nodes)),
		Edges: make([]*graph.Edge, 0, len(document.Edges)),
	}
	result.Nodes = append(result.Nodes, fileOnly.Nodes...)
	for index, pluginNode := range document.Nodes {
		if err := pollPluginContext(ctx, index); err != nil {
			return nil, err
		}
		node := e.toNode(filePath, pluginNode)
		if node != nil {
			result.Nodes = append(result.Nodes, node)
		}
	}
	for index, pluginEdge := range document.Edges {
		if err := pollPluginContext(ctx, index); err != nil {
			return nil, err
		}
		edge := e.toEdge(filePath, pluginEdge)
		if edge != nil {
			result.Edges = append(result.Edges, edge)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// toNode converts one plugin node into a graph.Node, applying sensible
// defaults. A node whose Kind is not a valid graph node kind is dropped
// (with a single logged warning per extractor instance) — an invalid
// kind must never poison the graph.
func (e *SubprocessExtractor) toNode(filePath string, pn pluginNode) *graph.Node {
	kind := graph.NodeKind(strings.TrimSpace(pn.Kind))
	if !graph.ValidNodeKind(kind) {
		e.dropOnce.Do(func() {
			e.log.Warn("extractor plugin: dropping node(s) with invalid kind",
				zap.String("language", e.language), zap.String("kind", pn.Kind))
		})
		return nil
	}
	fp := pn.FilePath
	if fp == "" {
		fp = filePath
	}
	lang := pn.Language
	if lang == "" {
		lang = e.language
	}
	id := strings.TrimSpace(pn.ID)
	if id == "" {
		id = fp + "::" + pn.Name
	}
	return &graph.Node{
		ID:        id,
		Kind:      kind,
		Name:      pn.Name,
		FilePath:  fp,
		StartLine: pn.StartLine,
		EndLine:   pn.EndLine,
		Language:  lang,
		Meta:      pn.Meta,
	}
}

// toEdge converts one plugin edge into a graph.Edge. An edge missing
// from/to/kind is dropped; the file path defaults to the source file.
func (e *SubprocessExtractor) toEdge(filePath string, pe pluginEdge) *graph.Edge {
	from := strings.TrimSpace(pe.From)
	to := strings.TrimSpace(pe.To)
	kind := strings.TrimSpace(pe.Kind)
	if from == "" || to == "" || kind == "" {
		return nil
	}
	fp := pe.FilePath
	if fp == "" {
		fp = filePath
	}
	return &graph.Edge{
		From:     from,
		To:       to,
		Kind:     graph.EdgeKind(kind),
		FilePath: fp,
		Line:     pe.Line,
		Meta:     pe.Meta,
	}
}

var (
	_ parser.Extractor        = (*SubprocessExtractor)(nil)
	_ parser.BoundedExtractor = (*SubprocessExtractor)(nil)
)

// RegisterExtractorPlugins registers every configured subprocess
// extractor plugin, mirroring RegisterCustomGrammars: a spec whose
// language or any extension collides with a built-in (or already
// registered) extractor is skipped — built-ins win — and a spec that
// fails to validate is skipped, each with a logged warning so a typo
// never aborts startup.
func RegisterExtractorPlugins(reg *parser.Registry, specs []config.ExtractorPluginSpec, log *zap.Logger) {
	if reg == nil {
		return
	}
	if log == nil {
		log = zap.NewNop()
	}
	for _, spec := range specs {
		lang := strings.TrimSpace(spec.Language)
		exts := normalizeGrammarExtensions(spec.Extensions)
		if lang == "" || strings.TrimSpace(spec.Command) == "" || len(exts) == 0 {
			log.Warn("extractor plugin: skipped — language, command and extensions are all required",
				zap.String("language", spec.Language), zap.String("command", spec.Command))
			continue
		}
		if _, exists := reg.GetByLanguage(lang); exists {
			log.Warn("extractor plugin: skipped — language already registered by a built-in extractor",
				zap.String("language", lang))
			continue
		}
		if conflict := firstClaimedExtension(reg, exts); conflict != "" {
			log.Warn("extractor plugin: skipped — extension already claimed by a built-in extractor",
				zap.String("language", lang), zap.String("extension", conflict))
			continue
		}
		spec.Extensions = exts
		reg.Register(NewSubprocessExtractor(spec, log))
		log.Info("extractor plugin registered",
			zap.String("language", lang), zap.Strings("extensions", exts),
			zap.String("command", spec.Command))
	}
}

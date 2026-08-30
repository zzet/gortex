package mcp

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// Capability requirements are request context in the same sense the view
// selector is: read and stripped by the middleware, so no handler sees them
// and no tool schema declares them.
//
// The three arguments compose one contract. require_complete expands to
// exactly the calling operation's defaults, required_capabilities adds to
// whatever that produced, and optional_capabilities only annotates. A
// capability that ends up required fails the request when the view cannot
// serve it; everything else rides back on the response.
const (
	requireCompleteArgName      = "require_complete"
	requiredCapabilitiesArgName = "required_capabilities"
	optionalCapabilitiesArgName = "optional_capabilities"
)

// capabilityRequest is the contract one request declared, already validated
// against the capability vocabulary.
type capabilityRequest struct {
	requireComplete bool
	required        []graphview.CapabilityID
	optional        []graphview.CapabilityID
}

// takeCapabilityRequest pulls the capability arguments off the request and
// removes them from the argument map.
//
// It runs beside takeViewSelector for the same reason: before parameter
// reconciliation, so the alias matcher cannot rewrite one of these names into
// a tool's own parameter, and before any handler runs, so every tool honours
// the contract without per-tool plumbing.
func takeCapabilityRequest(req *mcp.CallToolRequest) (capabilityRequest, error) {
	var want capabilityRequest
	if req == nil {
		return want, nil
	}
	args, ok := req.Params.Arguments.(map[string]any)
	if !ok {
		return want, nil
	}
	complete, err := takeCapabilityFlag(args)
	if err != nil {
		return capabilityRequest{}, err
	}
	want.requireComplete = complete
	if want.required, err = takeCapabilityList(args, requiredCapabilitiesArgName); err != nil {
		return capabilityRequest{}, err
	}
	if want.optional, err = takeCapabilityList(args, optionalCapabilitiesArgName); err != nil {
		return capabilityRequest{}, err
	}
	return want, nil
}

// takeCapabilityFlag reads and removes require_complete. A string form is
// accepted because clients that flatten arguments send booleans as text.
func takeCapabilityFlag(args map[string]any) (bool, error) {
	raw, present := args[requireCompleteArgName]
	if !present {
		return false, nil
	}
	delete(args, requireCompleteArgName)
	switch value := raw.(type) {
	case nil:
		return false, nil
	case bool:
		return value, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true":
			return true, nil
		case "false", "":
			return false, nil
		}
	}
	return false, graphview.NewViewError(graphview.CodeInvalidViewSelector,
		fmt.Sprintf("%s must be a boolean", requireCompleteArgName))
}

// takeCapabilityList reads and removes one capability-name list, refusing any
// name outside the vocabulary. A list is accepted as an array or as one
// comma-separated string, which is the shape a client that flattens arguments
// sends.
func takeCapabilityList(args map[string]any, name string) ([]graphview.CapabilityID, error) {
	raw, present := args[name]
	if !present {
		return nil, nil
	}
	delete(args, name)
	var names []string
	switch value := raw.(type) {
	case nil:
		return nil, nil
	case string:
		names = strings.FieldsFunc(value, func(r rune) bool { return r == ',' })
	case []string:
		names = value
	case []any:
		for _, item := range value {
			text, ok := item.(string)
			if !ok {
				return nil, graphview.NewViewError(graphview.CodeInvalidViewSelector,
					fmt.Sprintf("%s must be a list of capability names", name))
			}
			names = append(names, text)
		}
	default:
		return nil, graphview.NewViewError(graphview.CodeInvalidViewSelector,
			fmt.Sprintf("%s must be a list of capability names", name))
	}
	out := make([]graphview.CapabilityID, 0, len(names))
	for _, text := range names {
		if strings.TrimSpace(text) == "" {
			continue
		}
		id, err := graphview.ParseCapability(text)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// The capability families. Every read family names what an operation in it
// actually reads through, so a view that cannot serve one of them says so
// against the operation rather than against the whole surface.
var (
	// navigationCapabilities: a usage or traversal walks resolved references
	// in both directions, so it needs the syntax graph, the repository-local
	// resolution over it, and the reverse index that answers "who points at
	// this".
	navigationCapabilities = []graphview.CapabilityID{
		graphview.CapSyntaxGraph,
		graphview.CapResolutionLocal,
		graphview.CapIncomingEdges,
	}
	// searchCapabilities: symbol and content search over the view's own
	// corpora, plus the syntax graph every hit is described from.
	searchCapabilities = []graphview.CapabilityID{
		graphview.CapSearchSymbols,
		graphview.CapSearchContent,
		graphview.CapSyntaxGraph,
	}
	// readCapabilities: a file or symbol read answers from the view's
	// content snapshot and nothing else.
	readCapabilities = []graphview.CapabilityID{graphview.CapSourceSnapshot}
	// analysisCapabilities: an analyzer walks the syntax graph; the ones
	// that need more than that carry a per-tool override below.
	analysisCapabilities = []graphview.CapabilityID{graphview.CapSyntaxGraph}
)

// capabilityFamilyDefaults maps a tool family onto the capabilities an
// operation in that family requires.
//
// The families are the ones the surface already classifies itself by: a
// facade name from the operation table (the same table the mutation gate
// reads its effect classes out of), and otherwise the coarse tool category.
// Defaulting by family is what keeps this table small — enumerating 180 tools
// by hand would drift the day a tool is added, whereas a new tool lands in a
// family the moment it is registered.
//
// A family absent from the table declares nothing: mutations, session and
// workspace control, memories and the response post-filters are not reads of
// a view's content, so there is no capability for them to require.
var capabilityFamilyDefaults = map[string][]graphview.CapabilityID{
	"search":        searchCapabilities,
	"explore":       navigationCapabilities,
	"relations":     navigationCapabilities,
	"trace":         navigationCapabilities,
	toolCatNav:      navigationCapabilities,
	"read":          readCapabilities,
	"analyze":       analysisCapabilities,
	"ask":           analysisCapabilities,
	"change":        analysisCapabilities,
	"review":        analysisCapabilities,
	"pr":            analysisCapabilities,
	toolCatAnalysis: analysisCapabilities,
}

// toolCapabilityOverrides pins the capabilities of tools whose family is the
// wrong answer for them. Every entry is a case where the classification the
// surface already carries describes what the tool IS rather than what it
// reads.
var toolCapabilityOverrides = map[string][]graphview.CapabilityID{
	// The language-server tools are classified as edits and admin reads,
	// which is what they do — but what they read through is the language
	// server the view declares, and a view that ran none cannot serve them
	// at all.
	"get_diagnostics":   {graphview.CapLSPDiagnostics},
	"get_code_actions":  {graphview.CapLSPCodeActions},
	"apply_code_action": {graphview.CapLSPCodeActions},
	"fix_all_in_file":   {graphview.CapLSPCodeActions, graphview.CapLSPDiagnostics},
	// Search-family tools that do not read the symbol or content indexes.
	"search_text":      {graphview.CapSearchText},
	"search_ast":       {graphview.CapSourceSnapshot, graphview.CapSyntaxGraph},
	"find_files":       {graphview.CapSyntaxGraph},
	"search_artifacts": {graphview.CapSearchContent, graphview.CapSyntaxGraph},
	// An analyzer with a producer of its own behind it: near-duplicate
	// detection is a whole-corpus statistic, not a walk of the graph.
	"find_clones": {graphview.CapSyntaxGraph, graphview.CapSimilarity},
}

// legacyFacadeOnce memoizes the legacy-tool → facade index. The operation
// table is immutable, so one pass over it serves every request.
var (
	legacyFacadeOnce  sync.Once
	legacyFacadeIndex map[string]string
)

// facadeForLegacyTool names the facade an implementation-era tool belongs to.
// The first non-hidden mapping wins, which is the same canonical choice
// PublicOperationForLegacy makes for a handler with deliberate effect splits.
func facadeForLegacyTool(name string) (string, bool) {
	legacyFacadeOnce.Do(func() {
		legacyFacadeIndex = make(map[string]string)
		for _, spec := range facadeOperationSpecs() {
			if spec.Hidden {
				continue
			}
			if _, seen := legacyFacadeIndex[spec.Legacy]; !seen {
				legacyFacadeIndex[spec.Legacy] = spec.Facade
			}
		}
	})
	facade, ok := legacyFacadeIndex[name]
	return facade, ok
}

// capabilityToolName resolves the request onto the tool whose capabilities
// apply. A facade call names its operation rather than its implementation, so
// the operation is resolved to the legacy handler behind it — otherwise every
// operation of one facade would inherit the same defaults, and the four
// language-server operations would lose the only requirement that matters to
// them.
func (s *Server) capabilityToolName(req *mcp.CallToolRequest) string {
	name := req.Params.Name
	if !isFacadeToolName(name) {
		return name
	}
	args, ok := req.Params.Arguments.(map[string]any)
	if !ok {
		return name
	}
	raw, _ := args["operation"].(string)
	operation := resolveFacadeOperationAlias(name, normalizeFacadeOperation(raw))
	if spec, found := s.facades.operation(name, operation); found {
		return spec.Legacy
	}
	return name
}

// capabilityDefaultsFor is what require_complete expands to for one tool: a
// per-tool override when the family is the wrong answer, the family's
// defaults otherwise, and nothing at all for a family that reads no view.
func capabilityDefaultsFor(tool string) []graphview.CapabilityID {
	if caps, pinned := toolCapabilityOverrides[tool]; pinned {
		return caps
	}
	family := tool
	if !isFacadeToolName(tool) {
		if facade, mapped := facadeForLegacyTool(tool); mapped {
			family = facade
		} else {
			family = toolCategory(tool)
		}
	}
	return capabilityFamilyDefaults[family]
}

// evaluateRequestCapabilities enforces the request's capability contract
// before the handler runs.
//
// A required capability the view cannot serve at all refuses the request with
// capability_unavailable; one that is merely building or partial refuses it
// with required_capability_incomplete. Everything else — the operation's own
// defaults when the caller did not require them, and whatever it named
// optional — is evaluated too, but reported on the rider instead of refused.
//
// A request the base corpus serves is exempt. The base is a plain whole index
// with no producer rows to read, so its completeness is assumed complete this
// round; wiring it to the indexer's enrichment state is the successor to this
// and is what will make a base request able to fail here at all.
func (s *Server) evaluateRequestCapabilities(
	ctx context.Context,
	req *mcp.CallToolRequest,
	want capabilityRequest,
) *mcp.CallToolResult {
	view := requestViewFromContext(ctx)
	if view == nil || (!view.routed() && !view.baseFallback) {
		return nil
	}
	defaults := capabilityDefaultsFor(s.capabilityToolName(req))
	required := want.required
	annotate := want.optional
	if want.requireComplete {
		required = mergeCapabilities(defaults, required)
	} else {
		annotate = mergeCapabilities(defaults, annotate)
	}
	required = mergeCapabilities(required, nil)
	annotate = withoutCapabilities(mergeCapabilities(annotate, nil), required)
	if view.baseFallback {
		// search_text is allowed to resolve the sealed grace identity only so
		// its handler can return a labeled capability refusal. It reads no base
		// corpus, so reporting search.text as base-scoped would claim an answer
		// that never happened.
		if !s.requestNeedsGraceRefusalView(req) {
			view.noteBaseScoped(mergeCapabilities(required, annotate))
		}
		return nil
	}

	completeness := view.completeness()
	if err := completeness.Evaluate(required, annotate); err != nil {
		// The code says which of the two refusals it was — the view cannot
		// serve the capability at all, or it is still producing it — which is
		// the difference between a configuration problem and a wait.
		viewmetrics.Count(viewmetrics.CapabilityRefusedTotal, graphview.CodeOf(err))
		return mcp.NewToolResultError(fmt.Sprintf("%s: %s", req.Params.Name, err.Error()))
	}
	view.noteDegraded(completeness.Degraded(annotate))
	return nil
}

// mergeCapabilities concatenates two requirement sets, keeping first-seen
// order and dropping repeats, so a capability named twice is reported once.
func mergeCapabilities(first, second []graphview.CapabilityID) []graphview.CapabilityID {
	out := make([]graphview.CapabilityID, 0, len(first)+len(second))
	seen := make(map[graphview.CapabilityID]bool, len(first)+len(second))
	for _, id := range slices.Concat(first, second) {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// withoutCapabilities drops from caps everything already in exclude. A
// required capability is never also annotated: it either passed or the
// request is already refused.
func withoutCapabilities(caps, exclude []graphview.CapabilityID) []graphview.CapabilityID {
	if len(exclude) == 0 {
		return caps
	}
	out := make([]graphview.CapabilityID, 0, len(caps))
	for _, id := range caps {
		if !slices.Contains(exclude, id) {
			out = append(out, id)
		}
	}
	return out
}

// annotateBaseScoped records that a base-scoped engine produced part of this
// answer while a view other than the base served the request.
//
// Several analyzers build their own traversal state or their own index over
// the indexed corpus rather than over the composed reader, so under a routed
// view they answer about the base and not about the view the caller named. A
// comment saying so is invisible to the caller; this puts the same statement
// on the response, naming the capabilities that were answered from the base.
//
// It is a no-op on a base request, where reading the base IS the answer.
func annotateBaseScoped(ctx context.Context, caps ...graphview.CapabilityID) {
	view := requestViewFromContext(ctx)
	if view == nil || (!view.routed() && !view.baseFallback) {
		return
	}
	view.noteBaseScoped(caps)
}

// sortedCapabilityNames renders capability ids for the wire in a stable
// order, so a rider does not churn between identical requests.
func sortedCapabilityNames(caps []graphview.CapabilityID) []string {
	out := make([]string, 0, len(caps))
	for _, id := range caps {
		out = append(out, string(id))
	}
	sort.Strings(out)
	return out
}

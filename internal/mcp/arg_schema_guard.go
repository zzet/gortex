package mcp

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// #597: request decoding reads the keys it knows and unknown keys simply
// vanished — read_file(line_range: …) returned the full file, at maximum
// token cost, for a call that asked for a 40-line window, and the caller
// got no signal to self-correct. The guard makes dispatch surface what the
// published schema says: on a closed schema an unknown key appends an
// _ignored_options rider to the result by default, and under the reject
// opt-in it is an immediate tool error naming the key and the valid
// options, BEFORE the handler runs.

// toolArgGuardEnv dials enforcement for one run: unset (or anything
// unrecognised) warns — the handler runs and the result carries an
// _ignored_options rider; "reject" refuses the call before the handler,
// naming the unknown keys and the valid options; "0"/"false"/"off"/"no"
// restores the pre-guard behavior. Warn is the default because first-party
// surfaces inject undeclared keys into arbitrary tools (see
// toolArgShapingKeys) and third-party clients grew up against open
// schemas — reject-by-default would break calls that work today.
const toolArgGuardEnv = "GORTEX_TOOL_ARG_GUARD"

// toolArgShapingKeys are response-shaping options first-party surfaces
// inject into ANY tool call and generic layers honor without a per-tool
// declaration: the CLI pins format into every legacy-surface frame
// (buildToolCallFrameWithDefault), gortex call sets it for every non-facade
// tool, the HTTP bridge merges ?format= into any tool's args, and
// effectiveBudget / applyFieldsFilter read max_bytes / max_tokens / fields
// on every list-shaped response. The guard treats them as declared
// everywhere — warning on a key dispatch demonstrably honors would be
// noise, and rejecting it broke the CLI outright.
var toolArgShapingKeys = map[string]struct{}{
	"format":     {},
	"fields":     {},
	"max_bytes":  {},
	"max_tokens": {},
	"cursor":     {},
}

// The echoed unknown-key list is caller-controlled text: bound it in count
// and per-key length so the rider stays a nudge and the reject error stays
// an error, whatever the caller sent.
const (
	toolArgGuardMaxEchoedKeys = 5
	toolArgGuardMaxKeyRunes   = 80
)

// toolArgGuardEcho renders the capped unknown-key list shared by the rider
// and the reject error: at most toolArgGuardMaxEchoedKeys keys, each cut at
// toolArgGuardMaxKeyRunes runes, with an overflow tail naming the rest.
func toolArgGuardEcho(unknown []string) string {
	echo := make([]string, 0, len(unknown)+1)
	for i, k := range unknown {
		if i == toolArgGuardMaxEchoedKeys {
			echo = append(echo, fmt.Sprintf("(+%d more)", len(unknown)-toolArgGuardMaxEchoedKeys))
			break
		}
		if r := []rune(k); len(r) > toolArgGuardMaxKeyRunes {
			k = string(r[:toolArgGuardMaxKeyRunes]) + "…"
		}
		echo = append(echo, k)
	}
	return strings.Join(echo, ", ")
}

// argGuardPendingRider carries one warn verdict from the guard (which runs
// deep inside the handler chain) out to the dispatch wrapper, which attaches
// it AFTER the warming / freshness decorators: both rebuild the text result
// from Content[0] (rebuildTextResult), so a rider block appended mid-chain
// would be silently dropped exactly when the freshness rider fires — a file
// drifted on disk mid-edit, the case the guard exists for.
type argGuardPendingRider struct {
	text    string // the rider content block
	ignored string // capped key list, mirrored into StructuredContent
}

type argGuardRiderSlotKey struct{}

// withArgGuardRiderSlot arms the deferred rider attach for one dispatch.
func withArgGuardRiderSlot(ctx context.Context) context.Context {
	return context.WithValue(ctx, argGuardRiderSlotKey{}, &argGuardPendingRider{})
}

func pendingArgGuardRider(ctx context.Context) *argGuardPendingRider {
	p, _ := ctx.Value(argGuardRiderSlotKey{}).(*argGuardPendingRider)
	return p
}

// attachRiderToResult appends the rider content block and mirrors the capped
// key list into a structured payload that carries one. Error results never
// gain the rider: their error text stays clean.
func attachRiderToResult(res *mcp.CallToolResult, text, ignored string) {
	if res == nil || res.IsError || text == "" {
		return
	}
	res.Content = append(res.Content, mcp.NewTextContent(text))
	if sc, ok := res.StructuredContent.(map[string]any); ok {
		if _, taken := sc["_ignored_options"]; !taken {
			sc["_ignored_options"] = ignored
		}
	}
}

// attachPendingArgGuardRider lands the deferred rider at the end of the
// dispatch chain. It runs after the injection screen (sanitize wraps the
// handler, not the decorators), so the rider — whose key names are caller
// text — is screened here with the same detector; an existing security
// notice is never overwritten.
func (s *Server) attachPendingArgGuardRider(ctx context.Context, res *mcp.CallToolResult) *mcp.CallToolResult {
	pending := pendingArgGuardRider(ctx)
	if pending == nil || pending.text == "" {
		return res
	}
	attachRiderToResult(res, pending.text, pending.ignored)
	if s.sanitizeInjection && res != nil && !res.IsError {
		if hits := detectInjection(pending.text); len(hits) > 0 {
			if res.Meta == nil || res.Meta.AdditionalFields["gortex_security"] == nil {
				annotateSecurityMeta(res, nil, hits)
			}
		}
	}
	return res
}

// toolArgGuardKeys extracts a tool's declared top-level option names and
// whether its schema closes itself. Only an explicit
// additionalProperties:false on a structured schema closes it — one left
// open, explicitly or by JSON-Schema's permissive default, is honored in
// that direction too: no enforcement. Raw schemas are out of scope: no
// shipped tool uses one (the #597 stamp in prepareTool closes only
// structured schemas), so parsing raw JSON per registration would guard a
// population of zero.
func toolArgGuardKeys(tool mcp.Tool) (map[string]struct{}, bool) {
	if tool.RawInputSchema != nil {
		return nil, false
	}
	allowExtra, ok := tool.InputSchema.AdditionalProperties.(bool)
	if !ok || allowExtra {
		return nil, false
	}
	keys := make(map[string]struct{}, len(tool.InputSchema.Properties))
	for k := range tool.InputSchema.Properties {
		keys[k] = struct{}{}
	}
	return keys, true
}

// wrapToolArgGuard surfaces a closed schema's key set at dispatch. Open
// schemas pass through untouched.
func wrapToolArgGuard(tool mcp.Tool, handler server.ToolHandlerFunc) server.ToolHandlerFunc {
	allowed, closed := toolArgGuardKeys(tool)
	if !closed {
		return handler
	}
	valid := make([]string, 0, len(allowed))
	for k := range allowed {
		valid = append(valid, k)
	}
	sort.Strings(valid)
	validGloss := "valid options: " + strings.Join(valid, ", ")
	if len(valid) == 0 {
		validGloss = "this tool takes no options"
	}
	name := tool.Name
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mode := strings.ToLower(strings.TrimSpace(os.Getenv(toolArgGuardEnv)))
		switch mode {
		case "0", "false", "off", "no":
			return handler(ctx, req)
		}
		var unknown []string
		for k := range req.GetArguments() {
			if _, shaping := toolArgShapingKeys[k]; shaping {
				continue
			}
			if _, ok := allowed[k]; !ok {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) == 0 {
			return handler(ctx, req)
		}
		sort.Strings(unknown)
		ignored := toolArgGuardEcho(unknown)
		if mode == "reject" {
			return mcp.NewToolResultError(fmt.Sprintf(
				"%s does not accept option(s): %s; %s. The call was not executed — resend it with declared options only.",
				name, ignored, validGloss)), nil
		}
		res, err := handler(ctx, req)
		// The rider is a nudge on a successful result only: an error result
		// keeps its error text clean, and structured readers get the same
		// nudge mirrored into the structured payload — Content alone is
		// invisible to them. When the dispatch wrapper armed the deferred
		// slot, the rider is recorded there and attached after the
		// warming / freshness decorators — appending it here would hand it
		// to rebuildTextResult to drop. A slot-less call (direct handler
		// invocation) attaches inline, where no decorator runs.
		if err == nil && res != nil && !res.IsError {
			text := fmt.Sprintf("_ignored_options: %s — not options of %s; %s",
				ignored, name, validGloss)
			if slot := pendingArgGuardRider(ctx); slot != nil {
				slot.text, slot.ignored = text, ignored
			} else {
				attachRiderToResult(res, text, ignored)
			}
		}
		return res, err
	}
}

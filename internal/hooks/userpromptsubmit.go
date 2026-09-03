package hooks

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/profiles"
	"github.com/zzet/gortex/internal/toolref"
)

// UserPromptSubmitInput is the JSON Claude Code sends on UserPromptSubmit. We
// only consume the fields we use; unknown fields are ignored.
type UserPromptSubmitInput struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	CWD           string `json:"cwd"`
	Prompt        string `json:"prompt"`
}

// userPromptProbeTimeout bounds the pre-turn context probe. It runs on every
// user turn, so it must be fast and must never block the turn.
const userPromptProbeTimeout = 800 * time.Millisecond

// maxInjectedHits caps how many relevant symbols are injected per turn so the
// block stays a nudge, not a wall of text.
const maxInjectedHits = 6

// localizationProblemStatementMaxBytes bounds persisted prompt material. The
// framing protocol is fail-open: prompts outside this bound are never retained
// or substituted into tool input.
const localizationProblemStatementMaxBytes = 64 << 10

// framedLocalizationProblemStatement extracts the one explicitly labelled
// problem block from a submitted prompt. It intentionally has no raw-prompt
// fallback: absence, ambiguity, malformed framing, and overflow all leave the
// model-provided explore task unchanged.
func framedLocalizationProblemStatement(prompt string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(prompt, "\r\n", "\n"), "\n")
	openingIndex := -1
	var fence byte
	for lineIndex, line := range lines {
		candidateFence, ok := localizationProblemFrameOpening(line)
		if !ok {
			continue
		}
		if openingIndex >= 0 {
			return "", false
		}
		openingIndex = lineIndex
		fence = candidateFence
	}
	if openingIndex < 0 {
		return "", false
	}

	closerIndex := -1
	for lineIndex := openingIndex + 1; lineIndex < len(lines); lineIndex++ {
		if !localizationProblemFrameClosing(lines[lineIndex], fence) {
			continue
		}
		if closerIndex >= 0 {
			return "", false
		}
		closerIndex = lineIndex
	}
	if closerIndex < 0 {
		return "", false
	}

	start, end := openingIndex+1, closerIndex
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	if start == end {
		return "", false
	}

	statement := strings.Join(lines[start:end], "\n")
	if len(statement) > localizationProblemStatementMaxBytes {
		return "", false
	}
	return statement, true
}

func localizationProblemFrameOpening(line string) (byte, bool) {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 4 || !localizationProblemFenceByte(trimmed[0]) {
		return 0, false
	}
	fence := trimmed[0]
	leading := 0
	for leading < len(trimmed) && trimmed[leading] == fence {
		leading++
	}
	if leading < 3 {
		return 0, false
	}

	trailing := 0
	for trailing < len(trimmed)-leading && trimmed[len(trimmed)-1-trailing] == fence {
		trailing++
	}
	if trailing > 0 && trailing < 3 {
		return 0, false
	}
	labelEnd := len(trimmed)
	if trailing >= 3 {
		labelEnd -= trailing
	}
	label := strings.TrimSpace(trimmed[leading:labelEnd])
	if !localizationProblemFrameLabel(label) {
		return 0, false
	}
	return fence, true
}

func localizationProblemFrameLabel(label string) bool {
	words := strings.FieldsFunc(strings.ToLower(label), func(r rune) bool {
		return r < 'a' || r > 'z'
	})
	for wordIndex, word := range words {
		switch word {
		case "bug", "issue", "problem", "incident", "failure":
			return true
		case "change":
			if wordIndex+1 < len(words) && words[wordIndex+1] == "request" {
				return true
			}
		}
	}
	return false
}

func localizationProblemFrameClosing(line string, fence byte) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 16 {
		return false
	}
	for index := 0; index < len(trimmed); index++ {
		if trimmed[index] != fence {
			return false
		}
	}
	return true
}

func localizationProblemFenceByte(value byte) bool {
	return value == '-' || value == '='
}

var userPromptProbe grepProbeFn = probeViaDaemon

// runUserPromptSubmit handles a UserPromptSubmit hook: it proactively searches
// the graph for symbols relevant to the user's prompt and injects them as
// additionalContext, so the model reaches for Gortex's knowledge instead of
// blindly grepping. It is best-effort — any miss (daemon down, no hits, a
// trivial / non-code prompt) is a silent no-op so the turn is never blocked or
// polluted, and no warning is emitted (SessionStart already warns once when the
// daemon is down; doing so every turn would be noise).
func runUserPromptSubmit(data []byte) {
	started := time.Now()
	var input UserPromptSubmitInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	if input.HookEventName != "UserPromptSubmit" {
		return
	}
	// The user prompt is the authoritative turn boundary. Clear terminal
	// enforcement before any daemon-backed enrichment so a down daemon cannot
	// leave the previous turn's marker active.
	clearedTerminal := clearLocalizationTerminalFromHook(data)
	emitted := false
	defer func() {
		logHookEffectiveness("UserPromptSubmit", emitted, daemonReachableFn(), 0, time.Since(started))
		if clearedTerminal {
			localizationTerminalTelemetry("cleared_prompt", true, started)
		}
	}()
	block := buildUserPromptSubmitContext(input.HookEventName, input.Prompt)
	if block == "" {
		return
	}
	out, err := json.Marshal(HookOutput{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:     "UserPromptSubmit",
			AdditionalContext: block,
		},
	})
	if err != nil {
		return
	}
	emitted = true
	fmt.Print(string(out))
}

func buildUserPromptSubmitContext(eventName, prompt string) string {
	if eventName != "UserPromptSubmit" {
		return ""
	}
	query := promptQuery(prompt)
	if query == "" {
		return ""
	}
	// No scope: the prompt-time probe is orientation injected as context, not
	// a decision about a file, and its callers carry a prompt rather than a
	// location. It reads the base corpus, which is what it has always done.
	hits, err := userPromptProbe(query, "", userPromptProbeTimeout)
	if err != nil || len(hits) == 0 {
		return ""
	}
	return buildPromptInjection(hits)
}

// promptQuery normalizes a raw prompt into a search query, or "" when the
// prompt is too trivial / not code-related to bother probing. Slash commands
// and one-word acknowledgements ("ok", "go on") are skipped.
func promptQuery(prompt string) string {
	p := strings.TrimSpace(prompt)
	if p == "" || strings.HasPrefix(p, "/") {
		return ""
	}
	p = strings.Join(strings.Fields(p), " ") // collapse newlines / runs of spaces
	if r := []rune(p); len(r) > 240 {
		p = strings.TrimSpace(string(r[:240]))
	}
	// Require at least one token of length >= 3 so "ok", "yes" don't probe.
	for _, w := range strings.Fields(p) {
		if len(w) >= 3 {
			return p
		}
	}
	return ""
}

// maxInjectedHitsLean is the per-turn cap under the lean hook tier —
// the block keeps its cues but stops paying for marginal hits.
const maxInjectedHitsLean = 3

// buildPromptInjection renders the additionalContext block from search hits.
func buildPromptInjection(hits []grepSymbolHit) string {
	if len(hits) == 0 {
		return ""
	}
	lean := activeHookTier() == profiles.HookTierLean
	limit := maxInjectedHits
	if lean {
		limit = maxInjectedHitsLean
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	var sb strings.Builder
	sb.WriteString("## Gortex — relevant indexed symbols for your request\n\n")
	if !lean {
		sb.WriteString("Before reaching for grep/Read, these graph symbols look relevant to what you just asked:\n\n")
	}
	for _, h := range hits {
		kind := h.Kind
		if kind == "" {
			kind = "symbol"
		}
		if h.Line > 0 {
			fmt.Fprintf(&sb, "- `%s` (%s) — %s:%d\n", h.Name, kind, h.FilePath, h.Line)
		} else {
			fmt.Fprintf(&sb, "- `%s` (%s) — %s\n", h.Name, kind, h.FilePath)
		}
	}
	if lean {
		sb.WriteString("\nLeads, not the full picture — call `explore` with the request text for the ranked neighborhood " +
			"in one call; use `read(operation:\"source\")` for one symbol. Prefer these graph facts over grep/Read.\n")
	} else {
		sb.WriteString("\nThese are leads, not the full picture — call `explore` with the request text to get the ranked " +
			"neighborhood (these symbols and their siblings, WITH source + call paths + the files to change) in one call, " +
			"then answer or edit directly from it. Use `read(operation:\"source\")` for one symbol or operation `symbols` for a batch; " +
			"use `relations(operation:\"usages\")` for references or operation `callers` for callers. Prefer these graph facts over grep/Read.\n")
	}
	sb.WriteString(toolref.MCPRequiredLine())
	return sb.String()
}

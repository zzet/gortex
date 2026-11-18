package hooks

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"

	"github.com/zzet/gortex/internal/localizationauth"
	"github.com/zzet/gortex/internal/platform"
)

const (
	localizationTerminalMarkerVersion   = 3
	localizationTerminalContractV2      = 2
	localizationTerminalHostMetaVersion = 1
	localizationTerminalMarkerTTL       = 24 * time.Hour
	localizationTerminalPruneLimit      = 256
	localizationTerminalSessionHardCap  = 128
	localizationTerminalAgentHardCap    = 64
	localizationTerminalJanitorDeletes  = 32

	localizationTerminalContext    = "[Gortex] Localization for this task is complete. Answer now from completion.final_response, naming the files and symbols you rely on; if its evidence does not fit the request, say so and name what does. Either way, do not call another tool."
	localizationTerminalDenyReason = "[Gortex] Localization for this task is complete, so this tool call is blocked. Answer now from the retained evidence below, naming what you rely on; if it does not fit the request, say so in your answer."
	localizationAdvisoryDenyReason = "[Gortex] Localization for this task is complete, so this additional Gortex navigation call was not run. Answer now from the retained evidence below, naming what you rely on; if it does not fit the request, say so in your answer."
	gortexPluginMCPToolPrefix      = "mcp__plugin_gortex_gortex__"
	gortexCodexMCPToolPrefix       = "gortex__"
	localizationHostMetaKey        = "gortex/localization"
)

var localizationNavigationOperations = map[string]struct{}{
	"explore":   {},
	"search":    {},
	"read":      {},
	"relations": {},
	"trace":     {},
	"analyze":   {},
}

// localizationRedirectedHostTools are the host tools whose access-policy deny
// answers with "call a Gortex graph tool instead". While a localization marker
// is live that advice walks the caller straight into a refusal — measured as
// advise-then-refuse pairs, one wasted turn each — so the marker answers these
// tools itself rather than prescribing a call it will not honour.
var localizationRedirectedHostTools = map[string]struct{}{
	"Read": {},
	"Grep": {},
	"Glob": {},
}

func localizationRedirectedHostTool(tool string) bool {
	_, redirected := localizationRedirectedHostTools[tool]
	return redirected
}

var preToolUsePolicyTools = map[string]struct{}{
	"Read":  {},
	"Grep":  {},
	"Glob":  {},
	"Task":  {},
	"Bash":  {},
	"Edit":  {},
	"Write": {},
}

type localizationTerminalBase struct {
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id,omitempty"`
	CWD       string `json:"cwd"`
}

type localizationTerminalIdentity struct {
	SessionID string `json:"session_id"`
	PromptID  string `json:"prompt_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	CWD       string `json:"cwd"`
	TurnToken string `json:"turn_token"`
}

type localizationTurnState struct {
	Version          int                          `json:"version"`
	Identity         localizationTerminalIdentity `json:"identity"`
	ProblemStatement string                       `json:"problem_statement,omitempty"`
	CreatedUnixNano  int64                        `json:"created_unix_nano"`
}

type localizationToolSnapshot struct {
	Version         int                          `json:"version"`
	ToolUseID       string                       `json:"tool_use_id"`
	ToolName        string                       `json:"tool_name"`
	AuthToken       string                       `json:"auth_token,omitempty"`
	Identity        localizationTerminalIdentity `json:"identity"`
	CreatedUnixNano int64                        `json:"created_unix_nano"`
}

type localizationTerminalMarker struct {
	Version          int                          `json:"version"`
	ContractVersion  int                          `json:"contract_version"`
	Identity         localizationTerminalIdentity `json:"identity"`
	ObservedUnixNano int64                        `json:"observed_unix_nano"`
	Advisory         bool                         `json:"advisory,omitempty"`
	// FinalResponse rides the marker so a blocked call can hand back the answer
	// itself. A refusal that only says "you are done" gives the caller nothing
	// to act on and it tries the next tool; the answer ends the turn.
	FinalResponse      string   `json:"final_response,omitempty"`
	PrimaryIDs         []string `json:"primary_ids,omitempty"`
	EvidenceIDs        []string `json:"evidence_ids,omitempty"`
	ClaimCheckConsumed bool     `json:"claim_check_consumed,omitempty"`
}

type localizationTerminalHookInput struct {
	HookEventName    string                       `json:"hook_event_name"`
	ToolName         string                       `json:"tool_name"`
	ToolUseID        string                       `json:"tool_use_id"`
	ToolInput        map[string]any               `json:"tool_input"`
	SessionID        string                       `json:"session_id"`
	PromptID         string                       `json:"prompt_id"`
	AgentID          string                       `json:"agent_id"`
	CWD              string                       `json:"cwd"`
	Prompt           string                       `json:"prompt"`
	ToolResponse     json.RawMessage              `json:"tool_response"`
	TerminalReceipt  localizationauth.Receipt     `json:"-"`
	TerminalIdentity localizationTerminalIdentity `json:"-"`
}

type localizationTerminalCompletion struct {
	State            string `json:"state"`
	Scope            string `json:"scope"`
	RequiredAction   string `json:"required_action"`
	FinalResponse    string `json:"final_response,omitempty"`
	AllowedToolCalls *int   `json:"allowed_tool_calls"`
	ContractVersion  int    `json:"contract_version"`
	Enforceable      bool   `json:"enforceable"`
}

type localizationTerminalContract struct {
	Completion localizationTerminalCompletion `json:"completion"`
	Terminal   bool                           `json:"terminal"`
}

type localizationToolResponse struct {
	IsError                bool                       `json:"isError"`
	IsErrorSnake           bool                       `json:"is_error"`
	StructuredContent      json.RawMessage            `json:"structuredContent"`
	StructuredContentSnake json.RawMessage            `json:"structured_content"`
	Content                json.RawMessage            `json:"content"`
	Meta                   map[string]json.RawMessage `json:"_meta"`
}

type localizationTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type localizationHostEnvelope struct {
	Version  int                          `json:"version"`
	Contract localizationTerminalContract `json:"contract"`
	Evidence json.RawMessage              `json:"evidence"`
}

func observeLocalizationTerminal(data []byte) (localizationTerminalHookInput, bool) {
	var input localizationTerminalHookInput
	if err := json.Unmarshal(data, &input); err != nil || input.HookEventName != "PostToolUse" {
		return localizationTerminalHookInput{}, false
	}
	if !localizationNavigationTool(input.ToolName) {
		return localizationTerminalHookInput{}, false
	}
	identity, authToken, ok := consumeLocalizationToolSnapshot(input)
	if !ok || !localizationTerminalIdentityCurrent(identity) {
		return localizationTerminalHookInput{}, false
	}
	input.TerminalIdentity = identity
	if localizationProblemTaskRequest(input.ToolName, input.ToolInput) {
		_ = clearLocalizationProblemStatement(identity)
	}

	// Claude's real PostToolUse wire strips MCP _meta and structuredContent.
	// The one-shot receipt was published by the MCP server immediately before
	// returning answer_ready and is correlated through the authenticated
	// PreToolUse snapshot. Visible tool JSON is never an authority on this path.
	if authToken != "" {
		if receipt, authenticated := localizationauth.Consume(authToken); authenticated {
			if !markLocalizationTerminalReceipt(identity, receipt) {
				return localizationTerminalHookInput{}, false
			}
			input.TerminalReceipt = receipt
			return input, true
		}
	}

	// Retain compatibility with hosts that preserve the authoritative _meta
	// envelope. It still requires byte-equivalent visible and server metadata;
	// an arbitrary visible contract alone can never arm terminal state.
	contract, ok := exactLocalizationTerminalContract(input.ToolResponse)
	if !ok || !answerReadyLocalizationTerminalContract(contract) {
		return localizationTerminalHookInput{}, false
	}
	if !markLocalizationTerminalReceipt(identity, localizationauth.Receipt{
		FinalResponse: contract.Completion.FinalResponse, ContractVersion: contract.Completion.ContractVersion,
		Enforceable: contract.Completion.Enforceable,
	}) {
		return localizationTerminalHookInput{}, false
	}
	input.TerminalReceipt = localizationauth.Receipt{
		FinalResponse:   contract.Completion.FinalResponse,
		ContractVersion: contract.Completion.ContractVersion,
		Enforceable:     contract.Completion.Enforceable,
	}
	return input, true
}

// exactLocalizationTerminalContract accepts only a server-owned host envelope
// paired with the same contract in structuredContent or in one exact JSON text
// block. The authoritative metadata prevents repository text from arming the
// host gate; prefixes, extra blocks, and visible/metadata mismatches fail open.
func exactLocalizationTerminalContract(raw json.RawMessage) (localizationTerminalContract, bool) {
	raw, ok := unwrapJSONString(raw)
	if !ok {
		return localizationTerminalContract{}, false
	}
	var response localizationToolResponse
	if err := json.Unmarshal(raw, &response); err != nil || response.IsError || response.IsErrorSnake {
		return localizationTerminalContract{}, false
	}
	visible := response.StructuredContent
	if len(visible) == 0 {
		visible = response.StructuredContentSnake
	}
	if len(visible) > 0 {
		visible, ok = unwrapJSONString(visible)
	} else {
		visible, ok = exactLocalizationContractContent(response.Content)
	}
	if !ok || len(visible) == 0 {
		return localizationTerminalContract{}, false
	}
	var contract localizationTerminalContract
	if err := json.Unmarshal(visible, &contract); err != nil {
		return localizationTerminalContract{}, false
	}
	hostContract, ok := localizationHostContract(response.Meta)
	if !ok || !sameLocalizationTerminalContract(contract, hostContract) {
		return localizationTerminalContract{}, false
	}
	return contract, true
}

func localizationHostContract(meta map[string]json.RawMessage) (localizationTerminalContract, bool) {
	raw, ok := meta[localizationHostMetaKey]
	if !ok {
		return localizationTerminalContract{}, false
	}
	raw, ok = unwrapJSONString(raw)
	if !ok {
		return localizationTerminalContract{}, false
	}
	var envelope localizationHostEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Version != localizationTerminalHostMetaVersion {
		return localizationTerminalContract{}, false
	}
	if !answerReadyLocalizationTerminalContract(envelope.Contract) {
		return localizationTerminalContract{}, false
	}
	return envelope.Contract, true
}

func exactLocalizationContractContent(raw json.RawMessage) (json.RawMessage, bool) {
	raw, ok := unwrapJSONString(raw)
	if !ok || len(raw) == 0 {
		return nil, false
	}
	if raw[0] == '{' {
		var block localizationTextBlock
		if err := json.Unmarshal(raw, &block); err != nil || block.Type != "text" {
			return nil, false
		}
		return exactLocalizationContractText(block.Text)
	}
	if raw[0] != '[' {
		return nil, false
	}
	var blocks []localizationTextBlock
	if err := json.Unmarshal(raw, &blocks); err != nil || len(blocks) != 1 || blocks[0].Type != "text" {
		return nil, false
	}
	return exactLocalizationContractText(blocks[0].Text)
}

func exactLocalizationContractText(text string) (json.RawMessage, bool) {
	text = strings.TrimSpace(text)
	if text == "" || text[0] != '{' || !json.Valid([]byte(text)) {
		return nil, false
	}
	return json.RawMessage(text), true
}

func sameLocalizationTerminalContract(left, right localizationTerminalContract) bool {
	if left.Terminal != right.Terminal {
		return false
	}
	lc, rc := left.Completion, right.Completion
	if lc.State != rc.State || lc.Scope != rc.Scope || lc.RequiredAction != rc.RequiredAction ||
		lc.FinalResponse != rc.FinalResponse || lc.ContractVersion != rc.ContractVersion ||
		lc.Enforceable != rc.Enforceable {
		return false
	}
	if lc.AllowedToolCalls == nil || rc.AllowedToolCalls == nil {
		return lc.AllowedToolCalls == nil && rc.AllowedToolCalls == nil
	}
	return *lc.AllowedToolCalls == *rc.AllowedToolCalls
}

func unwrapJSONString(raw json.RawMessage) (json.RawMessage, bool) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return nil, false
	}
	for raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, false
		}
		raw = json.RawMessage(strings.TrimSpace(text))
		if len(raw) == 0 {
			return nil, false
		}
	}
	return raw, true
}

func answerReadyLocalizationTerminalContract(contract localizationTerminalContract) bool {
	completion := contract.Completion
	return contract.Terminal &&
		completion.State == "answer_ready" &&
		completion.Scope == "localization" &&
		completion.RequiredAction == "respond" &&
		strings.TrimSpace(completion.FinalResponse) != "" &&
		completion.AllowedToolCalls != nil && *completion.AllowedToolCalls == 0 &&
		completion.ContractVersion >= localizationTerminalContractV2
}

func localizationNavigationTool(tool string) bool {
	operation := shortGortexToolName(tool)
	if operation == tool {
		return false
	}
	_, ok := localizationNavigationOperations[operation]
	return ok
}

func isGortexMCPToolName(tool string) bool {
	return strings.HasPrefix(tool, gortexMCPToolPrefix) || strings.HasPrefix(tool, gortexPluginMCPToolPrefix) ||
		strings.HasPrefix(tool, gortexCodexMCPToolPrefix)
}

func preToolUsePolicyTool(tool string) bool {
	if isGortexMCPToolName(tool) {
		return true
	}
	_, ok := preToolUsePolicyTools[tool]
	return ok
}

func localizationPreToolUpdatedInput(input HookInput, authToken, problemStatement string) map[string]any {
	rewriteTask := localizationProblemTaskRewrite(input, problemStatement)
	if authToken == "" && !rewriteTask {
		return nil
	}
	updated := make(map[string]any, len(input.ToolInput)+1)
	for key, value := range input.ToolInput {
		updated[key] = value
	}
	if authToken != "" {
		updated[localizationauth.ArgumentKey] = authToken
	}
	if rewriteTask {
		updated["task"] = problemStatement
	}
	return updated
}

func localizationProblemTaskRewrite(input HookInput, problemStatement string) bool {
	if problemStatement == "" || !localizationProblemTaskRequest(input.ToolName, input.ToolInput) {
		return false
	}
	modelTask, _ := input.ToolInput["task"].(string)
	return len(strings.TrimSpace(modelTask))*2 < len(problemStatement)
}

func localizationProblemTaskRequest(toolName string, toolInput map[string]any) bool {
	if !isGortexMCPToolName(toolName) || shortGortexToolName(toolName) != "explore" {
		return false
	}
	operation, _ := toolInput["operation"].(string)
	if operation != "localize" {
		return false
	}
	options, ok := toolInput["options"].(map[string]any)
	if !ok {
		return false
	}
	newUserTask, _ := options["new_user_task"].(bool)
	return newUserTask
}

func localizationTerminalAdditionalContext(receipt localizationauth.Receipt) string {
	return localizationTerminalContext + "\n\n" + receipt.FinalResponse
}

func localizationTerminalBaseFor(sessionID, agentID, cwd string) (localizationTerminalBase, bool) {
	base := localizationTerminalBase{
		SessionID: strings.TrimSpace(sessionID),
		AgentID:   strings.TrimSpace(agentID),
		CWD:       canonicalLocalizationTerminalCWD(cwd),
	}
	return base, base.SessionID != "" && base.CWD != ""
}

func localizationTerminalRoot() string {
	dir := sessionStateDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "localization-terminal-v3")
}

func localizationStateHash(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func localizationSessionStateDir(base localizationTerminalBase) string {
	root := localizationTerminalRoot()
	key := struct {
		SessionID string `json:"session_id"`
		CWD       string `json:"cwd"`
	}{base.SessionID, base.CWD}
	digest := localizationStateHash(key)
	if root == "" || digest == "" {
		return ""
	}
	return filepath.Join(root, "sessions", digest)
}

func localizationAgentStateDir(base localizationTerminalBase) string {
	sessionDir := localizationSessionStateDir(base)
	digest := localizationStateHash(struct {
		AgentID string `json:"agent_id"`
	}{base.AgentID})
	if sessionDir == "" || digest == "" {
		return ""
	}
	return filepath.Join(sessionDir, "agents", digest)
}

func localizationTurnPath(base localizationTerminalBase) string {
	agentDir := localizationAgentStateDir(base)
	if agentDir == "" {
		return ""
	}
	return filepath.Join(agentDir, "turn.json")
}

func localizationSnapshotPath(base localizationTerminalBase, toolUseID string) string {
	agentDir := localizationAgentStateDir(base)
	toolUseID = strings.TrimSpace(toolUseID)
	digest := localizationStateHash(toolUseID)
	if agentDir == "" || toolUseID == "" || digest == "" {
		return ""
	}
	return filepath.Join(agentDir, "snapshots", digest+".json")
}

func localizationTerminalMarkerPath(identity localizationTerminalIdentity) string {
	base, ok := localizationTerminalBaseFor(identity.SessionID, identity.AgentID, identity.CWD)
	if !ok {
		return ""
	}
	agentDir := localizationAgentStateDir(base)
	digest := localizationStateHash(identity)
	if agentDir == "" || digest == "" {
		return ""
	}
	return filepath.Join(agentDir, "markers", digest+".json")
}

func localizationTerminalAdvisoryMarkerPath(identity localizationTerminalIdentity) string {
	path := localizationTerminalMarkerPath(identity)
	if path == "" {
		return ""
	}
	return strings.TrimSuffix(path, ".json") + ".advisory.json"
}

func beginLocalizationTurn(sessionID, promptID, agentID, cwd string) (localizationTerminalIdentity, bool) {
	return beginLocalizationTurnWithProblem(sessionID, promptID, agentID, cwd, "")
}

func beginLocalizationTurnWithProblem(sessionID, promptID, agentID, cwd, problemStatement string) (localizationTerminalIdentity, bool) {
	base, ok := localizationTerminalBaseFor(sessionID, agentID, cwd)
	if !ok {
		return localizationTerminalIdentity{}, false
	}
	var tokenBytes [16]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return localizationTerminalIdentity{}, false
	}
	identity := localizationTerminalIdentity{
		SessionID: base.SessionID,
		PromptID:  strings.TrimSpace(promptID),
		AgentID:   base.AgentID,
		CWD:       base.CWD,
		TurnToken: hex.EncodeToString(tokenBytes[:]),
	}
	state := localizationTurnState{
		Version:          localizationTerminalMarkerVersion,
		Identity:         identity,
		ProblemStatement: problemStatement,
		CreatedUnixNano:  time.Now().UnixNano(),
	}
	if !writeLocalizationState(localizationTurnPath(base), state) {
		return localizationTerminalIdentity{}, false
	}
	maintainLocalizationState(base)
	return identity, true
}

func currentLocalizationTurn(sessionID, promptID, agentID, cwd string) (localizationTerminalIdentity, bool) {
	state, ok := currentLocalizationTurnState(sessionID, promptID, agentID, cwd)
	if !ok {
		return localizationTerminalIdentity{}, false
	}
	return state.Identity, true
}

func currentLocalizationTurnState(sessionID, promptID, agentID, cwd string) (localizationTurnState, bool) {
	base, ok := localizationTerminalBaseFor(sessionID, agentID, cwd)
	if !ok {
		return localizationTurnState{}, false
	}
	var state localizationTurnState
	path := localizationTurnPath(base)
	if !readLocalizationState(path, &state) || !freshLocalizationTimestamp(path, state.CreatedUnixNano) ||
		state.Version != localizationTerminalMarkerVersion ||
		state.Identity.SessionID != base.SessionID || state.Identity.AgentID != base.AgentID || state.Identity.CWD != base.CWD ||
		strings.TrimSpace(state.Identity.TurnToken) == "" {
		return localizationTurnState{}, false
	}
	promptID = strings.TrimSpace(promptID)
	if promptID != "" && promptID != state.Identity.PromptID {
		return localizationTurnState{}, false
	}
	return state, true
}

func clearLocalizationProblemStatement(identity localizationTerminalIdentity) bool {
	state, ok := currentLocalizationTurnState(identity.SessionID, identity.PromptID, identity.AgentID, identity.CWD)
	if !ok || state.Identity != identity || state.ProblemStatement == "" {
		return false
	}
	state.ProblemStatement = ""
	base, ok := localizationTerminalBaseFor(identity.SessionID, identity.AgentID, identity.CWD)
	return ok && writeLocalizationState(localizationTurnPath(base), state)
}

func clearLocalizationProblemStatementForTurn(sessionID, promptID, agentID, cwd string) bool {
	state, ok := currentLocalizationTurnState(sessionID, promptID, agentID, cwd)
	if !ok {
		return false
	}
	return clearLocalizationProblemStatement(state.Identity)
}

func snapshotLocalizationToolUse(input HookInput, identity localizationTerminalIdentity) bool {
	_, ok := snapshotLocalizationToolUseWithAuth(input, identity)
	return ok
}

func snapshotLocalizationToolUseWithAuth(input HookInput, identity localizationTerminalIdentity) (string, bool) {
	if !localizationNavigationTool(input.ToolName) || strings.TrimSpace(input.ToolUseID) == "" {
		return "", false
	}
	base, ok := localizationTerminalBaseFor(input.SessionID, input.AgentID, input.CWD)
	if !ok {
		return "", false
	}
	authToken, ok := localizationauth.NewToken()
	if !ok {
		return "", false
	}
	snapshot := localizationToolSnapshot{
		Version:         localizationTerminalMarkerVersion,
		ToolUseID:       strings.TrimSpace(input.ToolUseID),
		ToolName:        input.ToolName,
		AuthToken:       authToken,
		Identity:        identity,
		CreatedUnixNano: time.Now().UnixNano(),
	}
	if !writeBoundedLocalizationState(localizationSnapshotPath(base, input.ToolUseID), snapshot) {
		return "", false
	}
	return authToken, true
}

func consumeLocalizationToolSnapshot(input localizationTerminalHookInput) (localizationTerminalIdentity, string, bool) {
	base, ok := localizationTerminalBaseFor(input.SessionID, input.AgentID, input.CWD)
	if !ok || strings.TrimSpace(input.ToolUseID) == "" {
		return localizationTerminalIdentity{}, "", false
	}
	path := localizationSnapshotPath(base, input.ToolUseID)
	var snapshot localizationToolSnapshot
	if !readLocalizationState(path, &snapshot) || !freshLocalizationTimestamp(path, snapshot.CreatedUnixNano) ||
		snapshot.Version != localizationTerminalMarkerVersion ||
		snapshot.ToolUseID != strings.TrimSpace(input.ToolUseID) || snapshot.ToolName != input.ToolName ||
		snapshot.Identity.SessionID != base.SessionID || snapshot.Identity.AgentID != base.AgentID || snapshot.Identity.CWD != base.CWD {
		return localizationTerminalIdentity{}, "", false
	}
	if promptID := strings.TrimSpace(input.PromptID); promptID != "" && promptID != snapshot.Identity.PromptID {
		return localizationTerminalIdentity{}, "", false
	}
	// Claiming the snapshot has to name exactly one winner. A rename does
	// that on POSIX, but two racing MoveFileEx calls on one source can both
	// fail on Windows and hand the turn to nobody, so the claim is an
	// exclusive marker that arbitrates identically on both.
	if !platform.ClaimFile(path) {
		return localizationTerminalIdentity{}, "", false
	}
	return snapshot.Identity, snapshot.AuthToken, true
}

func localizationTerminalIdentityCurrent(identity localizationTerminalIdentity) bool {
	state, ok := currentLocalizationTurnState(identity.SessionID, identity.PromptID, identity.AgentID, identity.CWD)
	return ok && state.Identity == identity
}

func markLocalizationTerminal(identity localizationTerminalIdentity, contractVersion int) bool {
	return markLocalizationTerminalWithStrength(identity, contractVersion, false, "")
}

func markLocalizationTerminalReceipt(identity localizationTerminalIdentity, receipt localizationauth.Receipt) bool {
	primaryIDs, evidenceIDs, ok := localizationauth.NormalizeEvidenceIDs(receipt.PrimaryIDs, receipt.EvidenceIDs)
	if !ok {
		return false
	}
	return markLocalizationTerminalWithEvidence(identity, receipt.ContractVersion, !receipt.Enforceable,
		receipt.FinalResponse, primaryIDs, evidenceIDs)
}

func markLocalizationTerminalWithStrength(
	identity localizationTerminalIdentity, contractVersion int, advisory bool, finalResponse string,
) bool {
	return markLocalizationTerminalWithEvidence(identity, contractVersion, advisory, finalResponse, nil, nil)
}

func markLocalizationTerminalWithEvidence(
	identity localizationTerminalIdentity, contractVersion int, advisory bool, finalResponse string,
	primaryIDs, evidenceIDs []string,
) bool {
	if identity.SessionID == "" || identity.CWD == "" || identity.TurnToken == "" ||
		contractVersion < localizationTerminalContractV2 {
		return false
	}
	marker := localizationTerminalMarker{
		Version:          localizationTerminalMarkerVersion,
		ContractVersion:  contractVersion,
		Identity:         identity,
		ObservedUnixNano: time.Now().UnixNano(),
		Advisory:         advisory,
		FinalResponse:    strings.TrimSpace(finalResponse),
		PrimaryIDs:       append([]string(nil), primaryIDs...),
		EvidenceIDs:      append([]string(nil), evidenceIDs...),
	}
	path := localizationTerminalMarkerPath(identity)
	if advisory {
		path = localizationTerminalAdvisoryMarkerPath(identity)
	}
	return writeBoundedLocalizationState(path, marker)
}

func localizationTerminalMarkerFor(identity localizationTerminalIdentity) (localizationTerminalMarker, bool) {
	if marker, ok := readLocalizationTerminalMarker(localizationTerminalMarkerPath(identity), identity); ok && !marker.Advisory {
		return marker, true
	}
	if marker, ok := readLocalizationTerminalMarker(localizationTerminalAdvisoryMarkerPath(identity), identity); ok && marker.Advisory {
		return marker, true
	}
	return localizationTerminalMarker{}, false
}

// consumeLocalizationClaimCheck serializes one enforceable terminal marker's
// challenge without ever removing the marker from its canonical path. Keeping
// that path readable closes the window in which a concurrent PreToolUse could
// bypass terminal enforcement. Host-retried Stop hooks still fail open after
// the first successful challenge, even when the host omits stop_hook_active.
func consumeLocalizationClaimCheck(identity localizationTerminalIdentity, claims []string) (localizationTerminalMarker, bool) {
	if len(claims) == 0 {
		return localizationTerminalMarker{}, false
	}
	path := localizationTerminalMarkerPath(identity)
	release, ok := acquireLocalizationClaimCheck(path)
	if !ok {
		return localizationTerminalMarker{}, false
	}
	defer release()

	marker, ok := readLocalizationTerminalMarker(path, identity)
	if !ok || marker.Advisory || marker.ClaimCheckConsumed || len(marker.PrimaryIDs) == 0 || len(marker.EvidenceIDs) == 0 {
		return localizationTerminalMarker{}, false
	}
	allAuthenticated := true
	for _, claim := range claims {
		matched := false
		for _, evidenceID := range marker.EvidenceIDs {
			if localizationClaimMatchesEvidence(claim, evidenceID) {
				matched = true
				break
			}
		}
		if !matched {
			allAuthenticated = false
			break
		}
	}
	if allAuthenticated {
		return localizationTerminalMarker{}, false
	}
	marker.ClaimCheckConsumed = true
	if !writeLocalizationState(path, marker) {
		return localizationTerminalMarker{}, false
	}
	return marker, true
}

func acquireLocalizationClaimCheck(path string) (func(), bool) {
	// Kernel-backed advisory locking makes the sidecar reusable: a process
	// crash releases ownership automatically, so a stale path can never wedge
	// future claim checks. Do not remove the sidecar on unlock; another process
	// may already have opened the same inode while waiting to contend.
	lock := flock.New(path + ".claim-check")
	locked, err := lock.TryLock()
	if err != nil || !locked {
		return nil, false
	}
	return func() { releaseLocalizationClaimLock(lock) }, true
}

const localizationClaimLockReleaseAttempts = 3

func releaseLocalizationClaimLock(lock *flock.Flock) {
	releaseLocalizationClaimLockObserved(lock, func(candidate *flock.Flock) error {
		// Close is gofrs/flock's documented release API: it unlocks and closes
		// the underlying descriptor while leaving the reusable sidecar in place.
		return candidate.Close()
	})
}

func releaseLocalizationClaimLockObserved(lock *flock.Flock, release func(*flock.Flock) error) bool {
	started := time.Now()
	if releaseLocalizationClaimLockWith(lock, release) {
		return true
	}
	// Lock-release telemetry describes an internal failure; no user-visible
	// hook context was emitted by this path.
	localizationTerminalTelemetry("claim_lock_release_failed", false, started)
	return false
}

func releaseLocalizationClaimLockWith(lock *flock.Flock, release func(*flock.Flock) error) bool {
	if lock == nil {
		return true
	}
	if release == nil {
		return false
	}
	for attempt := 0; attempt < localizationClaimLockReleaseAttempts; attempt++ {
		err := release(lock)
		if err == nil {
			return true
		}
		if !errors.Is(err, syscall.EINTR) {
			return false
		}
	}
	return false
}

func readLocalizationTerminalMarker(path string, identity localizationTerminalIdentity) (localizationTerminalMarker, bool) {
	var marker localizationTerminalMarker
	if !readLocalizationState(path, &marker) || !freshLocalizationTimestamp(path, marker.ObservedUnixNano) ||
		marker.Version != localizationTerminalMarkerVersion ||
		marker.ContractVersion < localizationTerminalContractV2 || marker.Identity != identity {
		return localizationTerminalMarker{}, false
	}
	return marker, true
}

func hasLocalizationTerminal(identity localizationTerminalIdentity) bool {
	marker, ok := localizationTerminalMarkerFor(identity)
	return ok && !marker.Advisory
}

func writeLocalizationState(path string, value any) bool {
	if path == "" {
		return false
	}
	data, err := json.Marshal(value)
	if err != nil {
		return false
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	tmp, err := os.CreateTemp(dir, ".localization-terminal-*")
	if err != nil {
		return false
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return false
	}
	if _, err := tmp.Write(data); err != nil {
		return false
	}
	if err := tmp.Sync(); err != nil {
		return false
	}
	if err := tmp.Close(); err != nil {
		return false
	}
	// A concurrent reader of the very state file being replaced makes the
	// commit fail with a sharing violation on Windows, and the hook would drop
	// a terminal marker it believed it had written; platform.ReplaceFile
	// retries that refusal briefly. Concurrent writers are serialized first:
	// on Windows every in-flight replace of the destination (and the scanner
	// handles each fresh replacement attracts) refuses all the others, so
	// unserialized writers keep invalidating one another's retries and a
	// backed-off writer can lose every attempt until its budget runs out.
	release := lockLocalizationStateDir(dir)
	err = platform.ReplaceFile(tmpPath, path)
	release()
	if err != nil {
		return false
	}
	committed = true
	return true
}

// localizationStateWriteLockName is the per-directory sidecar that serializes
// state commits. It is not a .json file, so trimStateDir never prunes it, and
// lifecycle rotation removes it with its namespace.
const localizationStateWriteLockName = ".state-write.lock"

const (
	// The wait outlasts platform.ReplaceFile's own retry budget, so a writer
	// queued behind one slow commit still gets its turn.
	localizationStateWriteLockWait  = 5 * time.Second
	localizationStateWriteLockDelay = 2 * time.Millisecond
)

// lockLocalizationStateDir takes the cross-process commit lock for dir and
// returns its release. The lock only orders writers; it is not needed for
// correctness, because each commit is an atomic replace. When the lock cannot
// be taken in time (or at all) the commit proceeds unserialized rather than
// dropping state.
func lockLocalizationStateDir(dir string) func() {
	lock := flock.New(filepath.Join(dir, localizationStateWriteLockName))
	ctx, cancel := context.WithTimeout(context.Background(), localizationStateWriteLockWait)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, localizationStateWriteLockDelay)
	if err != nil || !locked {
		_ = lock.Close()
		return func() {}
	}
	return func() { _ = lock.Close() }
}

// writeBoundedLocalizationState keeps transient marker/snapshot storage
// bounded within one session+agent namespace. Lifecycle rotation removes the
// entire namespace; this cap also handles missing PostToolUse/Stop callbacks.
func writeBoundedLocalizationState(path string, value any) bool {
	if !writeLocalizationState(path, value) {
		return false
	}
	trimStateDir(filepath.Dir(path), path, localizationTerminalMarkerTTL, localizationTerminalPruneLimit)
	return true
}

// trimStateDir bounds a directory of .json state files: anything older than
// ttl is removed outright, then the directory is pruned to hardCap oldest-
// first. preserve is never removed. Best-effort — every I/O error is ignored,
// because a hook must not fail over housekeeping.
func trimStateDir(dir, preserve string, ttl time.Duration, hardCap int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type stateFile struct {
		path    string
		modTime time.Time
	}
	files := make([]stateFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		age := time.Since(info.ModTime())
		if age < 0 || age > ttl {
			_ = os.Remove(path)
			continue
		}
		files = append(files, stateFile{path: path, modTime: info.ModTime()})
	}
	if len(files) <= hardCap {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].modTime.Equal(files[j].modTime) {
			return files[i].path < files[j].path
		}
		return files[i].modTime.Before(files[j].modTime)
	})
	remove := len(files) - hardCap
	for _, file := range files {
		if remove == 0 {
			break
		}
		if file.path == preserve {
			continue
		}
		if os.Remove(file.path) == nil {
			remove--
		}
	}
}

// maintainLocalizationState bounds abandoned lifecycle namespaces in addition
// to the transient files bounded above. The current session and agent are
// always preserved. Under normal operation each creation can exceed a cap by
// at most one, so one bounded janitor pass restores the hard cap immediately;
// a pre-existing oversized cache converges by a fixed deletion budget.
func maintainLocalizationState(base localizationTerminalBase) {
	sessionDir := localizationSessionStateDir(base)
	agentDir := localizationAgentStateDir(base)
	if sessionDir == "" || agentDir == "" {
		return
	}
	now := time.Now()
	_ = os.Chtimes(sessionDir, now, now)
	_ = os.Chtimes(agentDir, now, now)
	pruneLocalizationTreeDir(filepath.Join(localizationTerminalRoot(), "sessions"), sessionDir, localizationTerminalSessionHardCap)
	pruneLocalizationTreeDir(filepath.Join(sessionDir, "agents"), agentDir, localizationTerminalAgentHardCap)
}

func pruneLocalizationTreeDir(dir, preserve string, hardCap int) {
	entries, err := os.ReadDir(dir)
	if err != nil || hardCap < 1 {
		return
	}
	trees := make([]os.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			trees = append(trees, entry)
		}
	}
	if len(trees) == 0 {
		return
	}
	scanLimit := hardCap + localizationTerminalJanitorDeletes + 1
	if len(trees) < scanLimit {
		scanLimit = len(trees)
	}
	type stateTree struct {
		path    string
		modTime time.Time
	}
	fresh := make([]stateTree, 0, scanLimit)
	deleted := 0
	now := time.Now()
	for _, entry := range trees[:scanLimit] {
		path := filepath.Join(dir, entry.Name())
		if filepath.Clean(path) == filepath.Clean(preserve) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > localizationTerminalMarkerTTL && deleted < localizationTerminalJanitorDeletes {
			if os.RemoveAll(path) == nil {
				deleted++
			}
			continue
		}
		fresh = append(fresh, stateTree{path: path, modTime: info.ModTime()})
	}
	overCap := len(trees) - deleted - hardCap
	if overCap <= 0 || deleted >= localizationTerminalJanitorDeletes {
		return
	}
	sort.Slice(fresh, func(i, j int) bool {
		if fresh[i].modTime.Equal(fresh[j].modTime) {
			return fresh[i].path < fresh[j].path
		}
		return fresh[i].modTime.Before(fresh[j].modTime)
	})
	for _, tree := range fresh {
		if overCap == 0 || deleted >= localizationTerminalJanitorDeletes {
			break
		}
		if os.RemoveAll(tree.path) == nil {
			deleted++
			overCap--
		}
	}
}

func readLocalizationState(path string, value any) bool {
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(data, value) == nil
}

func freshLocalizationTimestamp(path string, timestamp int64) bool {
	if timestamp <= 0 {
		return false
	}
	age := time.Since(time.Unix(0, timestamp))
	if age < 0 || age > localizationTerminalMarkerTTL {
		_ = os.Remove(path)
		return false
	}
	return true
}

func clearLocalizationTerminalFromHook(data []byte) bool {
	var input localizationTerminalHookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return false
	}
	base, ok := localizationTerminalBaseFor(input.SessionID, input.AgentID, input.CWD)
	if !ok {
		return false
	}
	switch input.HookEventName {
	case "UserPromptSubmit":
		if strings.TrimSpace(input.AgentID) != "" {
			return false
		}
		removed, _ := discardLocalizationStateTree(localizationAgentStateDir(base))
		problemStatement, _ := framedLocalizationProblemStatement(input.Prompt)
		_, _ = beginLocalizationTurnWithProblem(input.SessionID, input.PromptID, input.AgentID, input.CWD, problemStatement)
		return removed
	case "SessionStart":
		path := localizationAgentStateDir(base)
		if input.AgentID == "" {
			path = localizationSessionStateDir(base)
		}
		removed, _ := discardLocalizationStateTree(path)
		return removed
	default:
		return false
	}
}

func beginLocalizationSubagentFromHook(data []byte) bool {
	var input localizationTerminalHookInput
	if err := json.Unmarshal(data, &input); err != nil || input.HookEventName != "SubagentStart" ||
		strings.TrimSpace(input.SessionID) == "" || strings.TrimSpace(input.AgentID) == "" || strings.TrimSpace(input.CWD) == "" {
		return false
	}
	base, ok := localizationTerminalBaseFor(input.SessionID, input.AgentID, input.CWD)
	if !ok {
		return false
	}
	_, _ = discardLocalizationStateTree(localizationAgentStateDir(base))
	_, ok = beginLocalizationTurn(input.SessionID, input.PromptID, input.AgentID, input.CWD)
	return ok
}

func endLocalizationSubagentFromHook(data []byte) bool {
	var input localizationTerminalHookInput
	if err := json.Unmarshal(data, &input); err != nil || input.HookEventName != "SubagentStop" ||
		strings.TrimSpace(input.SessionID) == "" || strings.TrimSpace(input.AgentID) == "" || strings.TrimSpace(input.CWD) == "" {
		return false
	}
	base, ok := localizationTerminalBaseFor(input.SessionID, input.AgentID, input.CWD)
	if !ok {
		return false
	}
	current, ok := currentLocalizationTurn(input.SessionID, input.PromptID, input.AgentID, input.CWD)
	if !ok || current.PromptID != strings.TrimSpace(input.PromptID) {
		return false
	}
	_, ok = discardLocalizationStateTree(localizationAgentStateDir(base))
	return ok
}

// discardLocalizationStateTree removes one exact session or agent namespace.
// Hash-derived paths make this both bounded and collision-resistant: cleanup
// never scans unrelated sessions and cannot starve behind an arbitrary entry
// limit. The bools report whether state existed and whether removal succeeded.
func discardLocalizationStateTree(path string) (bool, bool) {
	if path == "" {
		return false, false
	}
	_, err := os.Stat(path)
	existed := err == nil
	if err != nil && !os.IsNotExist(err) {
		return false, false
	}
	if err := os.RemoveAll(path); err != nil {
		return existed, false
	}
	return existed, true
}

func canonicalLocalizationTerminalCWD(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return filepath.Clean(cwd)
	}
	return filepath.Clean(abs)
}

func localizationTerminalTelemetry(event string, emitted bool, started time.Time) {
	logHookEffectivenessUnknown(fmt.Sprintf("LocalizationTerminal.%s", event), emitted, 0, time.Since(started))
}

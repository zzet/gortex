package hooks

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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

	localizationTerminalContext         = "Gortex localization is complete. Respond to the user now; do not call another tool in this turn."
	localizationTerminalDenyReason      = "[Gortex] Localization is complete. Respond to the user now; no further tool calls are allowed in this turn."
	localizationTerminalReplayDirective = "You already hold the localization answer — respond now using final_response. Do not call another tool."
	gortexPluginMCPToolPrefix      = "mcp__plugin_gortex_gortex__"
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
	Version         int                          `json:"version"`
	Identity        localizationTerminalIdentity `json:"identity"`
	CreatedUnixNano int64                        `json:"created_unix_nano"`
}

type localizationToolSnapshot struct {
	Version         int                          `json:"version"`
	ToolUseID       string                       `json:"tool_use_id"`
	ToolName        string                       `json:"tool_name"`
	Identity        localizationTerminalIdentity `json:"identity"`
	CreatedUnixNano int64                        `json:"created_unix_nano"`
}

type localizationTerminalMarker struct {
	Version          int                          `json:"version"`
	ContractVersion  int                          `json:"contract_version"`
	Identity         localizationTerminalIdentity `json:"identity"`
	ObservedUnixNano int64                        `json:"observed_unix_nano"`
}

type localizationTerminalHookInput struct {
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolUseID     string          `json:"tool_use_id"`
	SessionID     string          `json:"session_id"`
	PromptID      string          `json:"prompt_id"`
	AgentID       string          `json:"agent_id"`
	CWD           string          `json:"cwd"`
	ToolResponse  json.RawMessage `json:"tool_response"`
}

type localizationTerminalCompletion struct {
	State            string `json:"state"`
	Scope            string `json:"scope"`
	RequiredAction   string `json:"required_action"`
	Instruction      string `json:"instruction"`
	FinalResponse    string `json:"final_response"`
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
	Replay   bool                         `json:"replay"`
}

type localizationCompactReplay struct {
	Directive     string `json:"directive"`
	FinalResponse string `json:"final_response"`
}

func observeLocalizationTerminal(data []byte) (localizationTerminalHookInput, bool) {
	var input localizationTerminalHookInput
	if err := json.Unmarshal(data, &input); err != nil || input.HookEventName != "PostToolUse" {
		return localizationTerminalHookInput{}, false
	}
	if !localizationNavigationTool(input.ToolName) {
		return localizationTerminalHookInput{}, false
	}
	identity, ok := consumeLocalizationToolSnapshot(input)
	if !ok {
		return localizationTerminalHookInput{}, false
	}
	contract, ok := exactLocalizationTerminalContract(input.ToolResponse)
	if !ok || !enforceableLocalizationTerminalContract(contract) {
		return localizationTerminalHookInput{}, false
	}
	if !markLocalizationTerminal(identity, contract.Completion.ContractVersion) {
		return localizationTerminalHookInput{}, false
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
	structured := len(visible) > 0
	if structured {
		visible, ok = unwrapJSONString(visible)
	} else {
		visible, ok = exactLocalizationContractContent(response.Content)
	}
	if !ok || len(visible) == 0 {
		return localizationTerminalContract{}, false
	}
	var contract localizationTerminalContract
	if err := json.Unmarshal(visible, &contract); err == nil {
		hostContract, hostOK := localizationHostContract(response.Meta)
		if hostOK && sameLocalizationTerminalContract(contract, hostContract) {
			return contract, true
		}
	}
	if !structured {
		return localizationTerminalContract{}, false
	}
	compact, ok := exactLocalizationCompactReplay(visible)
	if !ok {
		return localizationTerminalContract{}, false
	}
	envelope, ok := localizationCompactReplayHostEnvelope(response.Meta)
	if !ok || compact.FinalResponse != envelope.Contract.Completion.FinalResponse {
		return localizationTerminalContract{}, false
	}
	return envelope.Contract, true
}

func exactLocalizationCompactReplay(raw json.RawMessage) (localizationCompactReplay, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 2 {
		return localizationCompactReplay{}, false
	}
	if _, ok := fields["directive"]; !ok {
		return localizationCompactReplay{}, false
	}
	if _, ok := fields["final_response"]; !ok {
		return localizationCompactReplay{}, false
	}
	var compact localizationCompactReplay
	if err := json.Unmarshal(raw, &compact); err != nil ||
		compact.Directive != localizationTerminalReplayDirective || compact.FinalResponse == "" {
		return localizationCompactReplay{}, false
	}
	return compact, true
}

func localizationCompactReplayHostEnvelope(meta map[string]json.RawMessage) (localizationHostEnvelope, bool) {
	raw, ok := meta[localizationHostMetaKey]
	if !ok {
		return localizationHostEnvelope{}, false
	}
	raw, ok = unwrapJSONString(raw)
	if !ok {
		return localizationHostEnvelope{}, false
	}
	var envelope localizationHostEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil ||
		envelope.Version != localizationTerminalHostMetaVersion || !envelope.Replay ||
		!enforceableLocalizationTerminalContract(envelope.Contract) {
		return localizationHostEnvelope{}, false
	}
	completion := envelope.Contract.Completion
	if completion.ContractVersion != localizationTerminalContractV2 ||
		completion.Instruction != localizationTerminalReplayDirective || completion.FinalResponse == "" {
		return localizationHostEnvelope{}, false
	}
	return envelope, true
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
	if !enforceableLocalizationTerminalContract(envelope.Contract) {
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
	if lc.AllowedToolCalls == nil || rc.AllowedToolCalls == nil {
		return lc.AllowedToolCalls == nil && rc.AllowedToolCalls == nil &&
			lc.State == rc.State && lc.Scope == rc.Scope && lc.RequiredAction == rc.RequiredAction &&
			lc.ContractVersion == rc.ContractVersion && lc.Enforceable == rc.Enforceable
	}
	return lc.State == rc.State && lc.Scope == rc.Scope && lc.RequiredAction == rc.RequiredAction &&
		*lc.AllowedToolCalls == *rc.AllowedToolCalls && lc.ContractVersion == rc.ContractVersion &&
		lc.Enforceable == rc.Enforceable
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

func enforceableLocalizationTerminalContract(contract localizationTerminalContract) bool {
	completion := contract.Completion
	return contract.Terminal &&
		completion.State == "answer_ready" &&
		completion.Scope == "localization" &&
		completion.RequiredAction == "respond" &&
		completion.AllowedToolCalls != nil && *completion.AllowedToolCalls == 0 &&
		completion.ContractVersion >= localizationTerminalContractV2 &&
		completion.Enforceable
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
	return strings.HasPrefix(tool, gortexMCPToolPrefix) || strings.HasPrefix(tool, gortexPluginMCPToolPrefix)
}

func preToolUsePolicyTool(tool string) bool {
	if isGortexMCPToolName(tool) {
		return true
	}
	_, ok := preToolUsePolicyTools[tool]
	return ok
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

func beginLocalizationTurn(sessionID, promptID, agentID, cwd string) (localizationTerminalIdentity, bool) {
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
		Version:         localizationTerminalMarkerVersion,
		Identity:        identity,
		CreatedUnixNano: time.Now().UnixNano(),
	}
	if !writeLocalizationState(localizationTurnPath(base), state) {
		return localizationTerminalIdentity{}, false
	}
	maintainLocalizationState(base)
	return identity, true
}

func currentLocalizationTurn(sessionID, promptID, agentID, cwd string) (localizationTerminalIdentity, bool) {
	base, ok := localizationTerminalBaseFor(sessionID, agentID, cwd)
	if !ok {
		return localizationTerminalIdentity{}, false
	}
	var state localizationTurnState
	path := localizationTurnPath(base)
	if !readLocalizationState(path, &state) || !freshLocalizationTimestamp(path, state.CreatedUnixNano) ||
		state.Version != localizationTerminalMarkerVersion ||
		state.Identity.SessionID != base.SessionID || state.Identity.AgentID != base.AgentID || state.Identity.CWD != base.CWD ||
		strings.TrimSpace(state.Identity.TurnToken) == "" {
		return localizationTerminalIdentity{}, false
	}
	promptID = strings.TrimSpace(promptID)
	if promptID != "" && promptID != state.Identity.PromptID {
		return localizationTerminalIdentity{}, false
	}
	return state.Identity, true
}

func snapshotLocalizationToolUse(input HookInput, identity localizationTerminalIdentity) bool {
	if !localizationNavigationTool(input.ToolName) || strings.TrimSpace(input.ToolUseID) == "" {
		return false
	}
	base, ok := localizationTerminalBaseFor(input.SessionID, input.AgentID, input.CWD)
	if !ok {
		return false
	}
	snapshot := localizationToolSnapshot{
		Version:         localizationTerminalMarkerVersion,
		ToolUseID:       strings.TrimSpace(input.ToolUseID),
		ToolName:        input.ToolName,
		Identity:        identity,
		CreatedUnixNano: time.Now().UnixNano(),
	}
	return writeBoundedLocalizationState(localizationSnapshotPath(base, input.ToolUseID), snapshot)
}

func consumeLocalizationToolSnapshot(input localizationTerminalHookInput) (localizationTerminalIdentity, bool) {
	base, ok := localizationTerminalBaseFor(input.SessionID, input.AgentID, input.CWD)
	if !ok || strings.TrimSpace(input.ToolUseID) == "" {
		return localizationTerminalIdentity{}, false
	}
	path := localizationSnapshotPath(base, input.ToolUseID)
	var snapshot localizationToolSnapshot
	if !readLocalizationState(path, &snapshot) || !freshLocalizationTimestamp(path, snapshot.CreatedUnixNano) ||
		snapshot.Version != localizationTerminalMarkerVersion ||
		snapshot.ToolUseID != strings.TrimSpace(input.ToolUseID) || snapshot.ToolName != input.ToolName ||
		snapshot.Identity.SessionID != base.SessionID || snapshot.Identity.AgentID != base.AgentID || snapshot.Identity.CWD != base.CWD {
		return localizationTerminalIdentity{}, false
	}
	_ = os.Remove(path)
	if promptID := strings.TrimSpace(input.PromptID); promptID != "" && promptID != snapshot.Identity.PromptID {
		return localizationTerminalIdentity{}, false
	}
	return snapshot.Identity, true
}

func markLocalizationTerminal(identity localizationTerminalIdentity, contractVersion int) bool {
	if identity.SessionID == "" || identity.CWD == "" || identity.TurnToken == "" ||
		contractVersion < localizationTerminalContractV2 {
		return false
	}
	marker := localizationTerminalMarker{
		Version:          localizationTerminalMarkerVersion,
		ContractVersion:  contractVersion,
		Identity:         identity,
		ObservedUnixNano: time.Now().UnixNano(),
	}
	return writeBoundedLocalizationState(localizationTerminalMarkerPath(identity), marker)
}

func hasLocalizationTerminal(identity localizationTerminalIdentity) bool {
	var marker localizationTerminalMarker
	path := localizationTerminalMarkerPath(identity)
	if !readLocalizationState(path, &marker) || !freshLocalizationTimestamp(path, marker.ObservedUnixNano) ||
		marker.Version != localizationTerminalMarkerVersion ||
		marker.ContractVersion < localizationTerminalContractV2 || marker.Identity != identity {
		return false
	}
	return true
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
	if err := os.Rename(tmpPath, path); err != nil {
		return false
	}
	committed = true
	return true
}

// writeBoundedLocalizationState keeps transient marker/snapshot storage
// bounded within one session+agent namespace. Lifecycle rotation removes the
// entire namespace; this cap also handles missing PostToolUse/Stop callbacks.
func writeBoundedLocalizationState(path string, value any) bool {
	if !writeLocalizationState(path, value) {
		return false
	}
	trimLocalizationStateDir(filepath.Dir(path), path)
	return true
}

func trimLocalizationStateDir(dir, preserve string) {
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
		if age < 0 || age > localizationTerminalMarkerTTL {
			_ = os.Remove(path)
			continue
		}
		files = append(files, stateFile{path: path, modTime: info.ModTime()})
	}
	if len(files) <= localizationTerminalPruneLimit {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].modTime.Equal(files[j].modTime) {
			return files[i].path < files[j].path
		}
		return files[i].modTime.Before(files[j].modTime)
	})
	remove := len(files) - localizationTerminalPruneLimit
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
		_, _ = beginLocalizationTurn(input.SessionID, input.PromptID, input.AgentID, input.CWD)
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

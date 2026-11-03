package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
)

type editProbeSharedMember struct {
	Receipt             string `json:"receipt"`
	PublicationKey      string `json:"publication_key"`
	Path                string `json:"path"`
	Ticket              uint64 `json:"ticket"`
	PhysicalSequence    uint64 `json:"physical_commit_sequence"`
	ContentSHA256       string `json:"content_sha256"`
	CheckoutID          string `json:"checkout_id"`
	CheckoutIncarnation string `json:"checkout_incarnation"`
}

type editProbeSharedProof struct {
	ownerKey   string
	definition json.RawMessage
	reference  json.RawMessage
	membership json.RawMessage
	member     editProbeSharedMember
	execution  *editProbeWindow
	background *editProbeWindow
}

func editProbeSharedHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func editProbeSharedReplyError(status map[string]any) error {
	for _, name := range []string{"truncated", "_truncated_by_budget", "_truncated_by_limit", "budget_exceeded", "budget_exhausted", "refused", "is_error"} {
		if value, present := status[name]; present && value != false {
			return errors.New("shared status has truncation/budget/refusal evidence: " + name)
		}
	}
	if value := status["error"]; value != nil && value != "" {
		return errors.New("shared status has error evidence")
	}
	return nil
}

func decodeEditProbeSharedMember(value any, binding *editProbeSharedBinding) (editProbeSharedMember, error) {
	object, _ := value.(map[string]any)
	if !editProbeExactKeys(object, "receipt", "publication_key", "path", "ticket", "physical_commit_sequence", "content_sha256", "checkout_id", "checkout_incarnation") {
		return editProbeSharedMember{}, errors.New("shared physical member shape unavailable")
	}
	var m editProbeSharedMember
	var ok bool
	m.Receipt, ok = object["receipt"].(string)
	if !ok || m.Receipt == "" {
		return m, errors.New("shared physical receipt unavailable")
	}
	m.PublicationKey, ok = object["publication_key"].(string)
	if !ok || m.PublicationKey == "" {
		return m, errors.New("shared publication key unavailable")
	}
	m.Path, ok = object["path"].(string)
	if !ok || binding == nil || !filepath.IsAbs(m.Path) || filepath.Clean(m.Path) != m.Path || m.Path == binding.root || !inside(binding.root, m.Path) {
		return m, errors.New("shared member is outside its actual primary checkout")
	}
	m.ContentSHA256, ok = object["content_sha256"].(string)
	if !ok || !editProbeSharedHash(m.ContentSHA256) {
		return m, errors.New("shared original SHA256 unavailable")
	}
	m.CheckoutID, ok = object["checkout_id"].(string)
	if !ok || m.CheckoutID != "" {
		return m, errors.New("shared member routed checkout is unsupported")
	}
	m.CheckoutIncarnation, ok = object["checkout_incarnation"].(string)
	if !ok || m.CheckoutIncarnation != "" {
		return m, errors.New("shared member routed incarnation is unsupported")
	}
	var err error
	m.Ticket, err = strconvEditProbeUint(object["ticket"])
	if err != nil || m.Ticket == 0 {
		return m, errors.New("shared member original ticket unavailable")
	}
	m.PhysicalSequence, err = strconvEditProbeUint(object["physical_commit_sequence"])
	if err != nil || m.PhysicalSequence == 0 {
		return m, errors.New("shared member original physical sequence unavailable")
	}
	return m, nil
}

func validateEditProbeSharedOwner(owner map[string]any, binding *editProbeSharedBinding) (string, uint64, uint64, error) {
	bad := func() (string, uint64, uint64, error) {
		return "", 0, 0, errors.New("shared owner/domain does not identify the actual owned arm")
	}
	if binding == nil || !editProbeExactKeys(owner, "process", "server_instance", "recorder_instance", "checkout", "anchor_publication_key", "anchor_ticket", "retry", "invocation_anchor_key", "invocation_anchor_ticket", "arm_frontier") {
		return bad()
	}
	process, _ := owner["process"].(map[string]any)
	pid, pidErr := strconvEditProbeUint(process["pid"])
	if !editProbeExactKeys(process, "pid", "birth_token") || pidErr != nil || pid != uint64(binding.identity.PID()) || process["birth_token"] != binding.birth {
		return bad()
	}
	for _, name := range []string{"server_instance", "recorder_instance"} {
		if value, ok := owner[name].(string); !ok || value == "" {
			return bad()
		}
	}
	checkout, _ := owner["checkout"].(map[string]any)
	if !editProbeExactKeys(checkout, "kind", "root", "id", "incarnation", "repo_prefix") || checkout["kind"] != "watcher_primary" || checkout["root"] != binding.root || checkout["repo_prefix"] != binding.prefix || checkout["id"] != "" || checkout["incarnation"] != "" {
		return bad()
	}
	anchor, ok := owner["anchor_publication_key"].(string)
	ticket, ticketErr := strconvEditProbeUint(owner["anchor_ticket"])
	invocationTicket, invocationErr := strconvEditProbeUint(owner["invocation_anchor_ticket"])
	retry, retryErr := strconvEditProbeUint(owner["retry"])
	frontier, frontierErr := strconvEditProbeUint(owner["arm_frontier"])
	if !ok || anchor == "" || ticketErr != nil || ticket == 0 || invocationErr != nil || invocationTicket != ticket || owner["invocation_anchor_key"] != anchor || retryErr != nil || retry != 0 || frontierErr != nil || frontier < ticket {
		return bad()
	}
	return anchor, ticket, frontier, nil
}

// The returned bytes are immutable report data, not a replacement for the
// same-status witness. A dictionary may deduplicate them only after this full
// check against the actual live owned child; it cannot revive a later reply.
func checkEditProbeSharedProof(status map[string]any, receipt string, binding *editProbeSharedBinding, p *privateRun, role string) (editProbeSharedProof, error) {
	var out editProbeSharedProof
	bad := func(reason string) (editProbeSharedProof, error) {
		if out.ownerKey != "" {
			if err := binding.validate(p, role); err != nil {
				return editProbeSharedProof{}, err
			}
		}
		return out, errors.New(reason)
	}
	if err := binding.validate(p, role); err != nil {
		return bad(err.Error())
	}
	if err := editProbeSharedReplyError(status); err != nil {
		return bad(err.Error())
	}
	for name := range status {
		if strings.HasPrefix(name, "shared_owner_accounting_v") && name != "shared_owner_accounting_v1" && name != "shared_owner_accounting_v1_availability" {
			return bad("unknown shared owner proof version")
		}
	}
	availability, _ := status["shared_owner_accounting_v1_availability"].(map[string]any)
	if !editProbeExactKeys(availability, "state", "reason") || availability["state"] != "frozen" || availability["reason"] != nil {
		return bad("shared owner definition is not frozen/current")
	}
	proof, _ := status["shared_owner_accounting_v1"].(map[string]any)
	if !editProbeExactKeys(proof, "reference", "definition", "membership") {
		return bad("shared owner proof is not self-contained")
	}
	definition, _ := proof["definition"].(map[string]any)
	if !editProbeExactKeys(definition, "scope", "owner", "frontier_count", "members", "execution", "background", "background_unavailable_reason") || definition["scope"] != "shared_mutation_set_executor" {
		return bad("shared owner definition shape/scope unavailable")
	}
	owner, _ := definition["owner"].(map[string]any)
	anchor, anchorTicket, frontier, err := validateEditProbeSharedOwner(owner, binding)
	if err != nil {
		return bad(err.Error())
	}
	count, countErr := strconvEditProbeUint(definition["frontier_count"])
	values, ok := definition["members"].([]any)
	if countErr != nil || count == 0 || !ok || count != uint64(len(values)) {
		return bad("shared complete frontier/member count differs")
	}
	seenPaths, seenKeys, seenReceipts := map[string]bool{}, map[string]bool{}, map[string]bool{}
	seenTickets, seenSequences := map[uint64]bool{}, map[uint64]bool{}
	members := make([]editProbeSharedMember, 0, len(values))
	anchorFound := false
	for _, value := range values {
		m, err := decodeEditProbeSharedMember(value, binding)
		if err != nil || seenPaths[m.Path] || seenKeys[m.PublicationKey] || seenReceipts[m.Receipt] || seenTickets[m.Ticket] || seenSequences[m.PhysicalSequence] || m.Ticket > frontier || len(members) > 0 && members[len(members)-1].Path >= m.Path {
			return bad("shared original full frontier has invalid, repeated or unordered members")
		}
		seenPaths[m.Path], seenKeys[m.PublicationKey], seenReceipts[m.Receipt], seenTickets[m.Ticket], seenSequences[m.PhysicalSequence] = true, true, true, true, true
		anchorFound = anchorFound || m.PublicationKey == anchor && m.Ticket == anchorTicket
		members = append(members, m)
	}
	if !anchorFound {
		return bad("shared invocation anchor is outside the original full frontier")
	}
	reference, _ := proof["reference"].(map[string]any)
	membership, _ := proof["membership"].(map[string]any)
	if !editProbeExactKeys(reference, "owner", "member") || !reflect.DeepEqual(reference["owner"], owner) ||
		!editProbeExactKeys(membership, "owner", "member", "applied_generation", "same_status_projection", "live_server_ledger_member", "live_recorder_member", "live_anchor", "full_membership_retained", "original_invocation_verified", "publication_terminal_success") || !reflect.DeepEqual(membership["owner"], owner) {
		return bad("shared reference/current membership has a different owner")
	}
	for _, name := range []string{"same_status_projection", "live_server_ledger_member", "live_recorder_member", "live_anchor", "full_membership_retained", "original_invocation_verified", "publication_terminal_success"} {
		if membership[name] != true {
			return bad("shared current complete membership witness unavailable: " + name)
		}
	}
	m, err := decodeEditProbeSharedMember(reference["member"], binding)
	witness, witnessErr := decodeEditProbeSharedMember(membership["member"], binding)
	found := false
	for _, member := range members {
		found = found || member == m
	}
	if err != nil || witnessErr != nil || witness != m || !found || m.Receipt != receipt || status["receipt"] != receipt || status["found"] != true || status["disk_status"] != "committed" || status["graph_status"] != "fresh" || status["graph_status_terminal"] != true {
		return bad("shared current receipt does not identify its exact original member")
	}
	relative, relErr := filepath.Rel(binding.root, m.Path)
	label, labelOK := status["path"].(string)
	if relErr != nil || !labelOK || label != m.Path && label != filepath.ToSlash(relative) && label != filepath.ToSlash(filepath.Join(binding.prefix, relative)) ||
		status["checkout_id"] != nil && status["checkout_id"] != "" || status["checkout_incarnation"] != nil && status["checkout_incarnation"] != "" || status["receipt_kind"] != nil {
		return bad("shared status path/checkout/kind differs from the original physical member")
	}
	requested, reqErr := strconvEditProbeUint(status["reindex_generation"])
	applied, appErr := strconvEditProbeUint(status["applied_generation"])
	witnessApplied, waErr := strconvEditProbeUint(membership["applied_generation"])
	sequence, seqErr := strconvEditProbeUint(status["commit_seq"])
	publication, _ := status["publication_phases"].(map[string]any)
	pubTicket, pubErr := strconvEditProbeUint(publication["ticket"])
	if reqErr != nil || appErr != nil || waErr != nil || seqErr != nil || pubErr != nil || requested != m.Ticket || applied != m.Ticket || witnessApplied != m.Ticket || pubTicket != m.Ticket || sequence != m.PhysicalSequence || status["content_sha256"] != m.ContentSHA256 || publication["key"] != m.PublicationKey || publication["source"] != "mcp_edit" || publication["clock"] != "monotonic" || publication["terminal"] != true {
		return bad("shared receipt/publication/physical attempt identities disagree")
	}
	physical, _ := status["phase_timing"].(map[string]any)
	if physical["patch_wall_ms"] != nil || physical["patch_accounting"] != nil || status["requested_background"] != nil || status["coalesced_publication"] != nil {
		return bad("shared execution cannot also claim legacy per-file P/B or a coalesced winner")
	}
	ownerBytes, err := json.Marshal(struct {
		Role, Product string
		StartCount    int
		Owner         map[string]any
	}{role, binding.product, binding.startCount, owner})
	if err != nil {
		return bad("shared owner normalization failed")
	}
	digest := sha256.Sum256(ownerBytes)
	out.ownerKey = hex.EncodeToString(digest[:]) // report index only, never membership authority
	out.definition, err = json.Marshal(definition)
	if err != nil {
		return bad("shared definition normalization failed")
	}
	out.reference, err = json.Marshal(reference)
	if err != nil {
		return bad("shared reference normalization failed")
	}
	out.membership, err = json.Marshal(membership)
	if err != nil {
		return bad("shared same-status witness normalization failed")
	}
	out.member = m
	execution, _ := definition["execution"].(map[string]any)
	e, err := checkEditProbeSharedExecution(execution, len(members))
	if err != nil {
		return bad(err.Error())
	}
	out.execution = &e
	if definition["background"] == nil {
		if reason, ok := definition["background_unavailable_reason"].(string); !ok || reason == "" {
			return bad("shared final B is absent without its actual unavailability reason")
		}
		return bad("shared final background is explicitly unavailable")
	}
	if definition["background_unavailable_reason"] != nil {
		return bad("shared background has a contradictory availability reason")
	}
	background, _ := definition["background"].(map[string]any)
	b, err := checkEditProbeSharedBackground(background, owner, e)
	if err != nil {
		return bad(err.Error())
	}
	out.background = &b
	if err := binding.validate(p, role); err != nil {
		return bad(err.Error())
	}
	return out, nil
}

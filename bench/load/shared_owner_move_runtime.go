package main

import "context"

func (r *editProbeRuntime) accountingForMoveStatuses(ctx context.Context, statuses map[string]map[string]any, refs editProbeMoveReferences, wireID int64) editProbeMoveAccounting {
	out := checkEditProbeMoveAccounting(statuses, refs, wireID)
	shared := false
	for _, row := range statuses {
		child, _ := row["physical_commit"].(map[string]any)
		shared = shared || editProbeHasSharedOwnerFields(child)
	}
	if !shared {
		return out // historical direct and qualified ACK-only evidence stay exact
	}
	qualifiedACK := out.State == "qualified_queued_ack_write_only"
	out.State = "unavailable_move_shared_owner"
	for i := range out.Files {
		out.Files[i].D, out.Files[i].R = nil, nil
	}
	if !qualifiedACK {
		out.Reasons = append(out.Reasons, "shared owner requires independently validated queued move ACK/write evidence")
		return out
	}
	// The legacy reason is only the explicit absence of per-file P/B. Replace
	// it with the distinct owner proof; those per-file windows remain absent.
	out.Reasons = nil
	for i := range out.Files {
		file := &out.Files[i]
		row := statuses[file.Role]
		if err := editProbeSharedReplyError(row); err != nil {
			out.A = nil
			out.Reasons = append(out.Reasons, "move parent: "+file.Role+": "+err.Error())
			continue
		}
		child, _ := row["physical_commit"].(map[string]any)
		parentACK, parentErr := hashJSON(row["acknowledgement"])
		childACK, childErr := hashJSON(child["acknowledgement"])
		if row["physical_commit_state"] != "observed" || row["physical_commit_shared_acknowledgement"] != true || row["physical_commit_additive_with_acknowledgement"] != false || row["requested_background_state"] != "admitted" ||
			child["new_sha"] != file.Blob || parentErr != nil || childErr != nil || parentACK != childACK || !editProbeHasSharedOwnerFields(child) {
			out.Reasons = append(out.Reasons, "move physical child has no exact original blob/shared ACK/nonadditive link: "+file.Role)
			continue
		}
		receipt, _ := child["receipt"].(string)
		ref, _, err := r.referenceForSharedStatus(ctx, child, receipt, file.Path)
		if err != nil {
			out.Reasons = append(out.Reasons, "move physical child: "+file.Role+": "+err.Error())
			continue
		}
		file.Shared, file.Route = ref, "queued_publication_shared_owner_windows"
	}
	if len(out.Reasons) == 0 && len(out.Files) == len(refs.Files) {
		out.State = "observed_move_shared_owner_windows_checks_pass"
	}
	return out
}

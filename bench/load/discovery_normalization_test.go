//go:build linux || darwin

package main

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These are fresh byte/parser fixtures, not real SQLite databases, native
// normalizations, or upgrade acceptance. No retained data or SQL is opened.
type normalizationFixture struct {
	options runOptions
	corpora []Corpus
	lineage discoverySeedLineage
	docs    map[string]map[string]any
	proof   map[string]any
}

func cloneNormalizationMap(t *testing.T, in map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func syntheticNormalization(t *testing.T) *normalizationFixture {
	t.Helper()
	o, corpora, l := syntheticDiscoverySeed(t)
	root := l.OwnedRoot
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b := []byte("synthetic original sidecar" + suffix)
		if suffix == "" {
			b = make([]byte, 100)
			copy(b, "SQLite format 3\x00")
			binary.BigEndian.PutUint32(b[60:64], 21)
		}
		require.NoError(t, os.WriteFile(filepath.Join(root, "store.db")+suffix, b, 0600))
	}
	sourceSHA := func(suffix string) string {
		x, err := discoveryFileSHA(filepath.Join(root, "store.db") + suffix)
		require.NoError(t, err)
		return x
	}
	source := l
	source.MainSHA256 = sourceSHA("")
	source.WALSHA256 = sourceSHA("-wal")
	rewriteSyntheticDiscoveryProofs(t, &source)
	l.StopReceiptSHA256 = source.StopReceiptSHA256
	l.IdentityReceiptSHA256 = source.IdentityReceiptSHA256
	var stop discoverySeedProof
	require.NoError(t, discoveryReadJSON(l.StopReceipt, &stop))
	var catalog discoveryCatalogReceipt
	require.NoError(t, discoveryReadJSON(stop.CatalogReceipt, &catalog))
	expected, err := discoveryCatalogIdentities(catalog.After, l.Repos)
	require.NoError(t, err)
	copyRoot := filepath.Join(root, "normalization", "copy")
	require.NoError(t, os.MkdirAll(copyRoot, 0700))
	data, err := os.ReadFile(l.Snapshot)
	require.NoError(t, err)
	l.Snapshot = filepath.Join(copyRoot, "store.db")
	require.NoError(t, os.WriteFile(l.Snapshot, data, 0600))
	native := exec.Command("sh", "-c", "exit 0")
	require.NoError(t, native.Run())
	pid := native.Process.Pid
	begin := stop.ObservedAt.Add(time.Second)
	end := begin.Add(time.Second)
	stamp := func(x time.Time) string { return x.UTC().Format(time.RFC3339Nano) }
	rows := map[string][]map[string]any{"repository_families": {}, "checkouts": {}, "tracking_intents": {}, "dedicated_graphs": {}}
	seen := map[string]bool{}
	for _, scope := range catalog.After {
		for _, family := range scope.Overview.Families {
			if seen[family.FamilyID] {
				continue
			}
			seen[family.FamilyID] = true
			rows["repository_families"] = append(rows["repository_families"], map[string]any{"family_id": family.FamilyID, "common_dir_identity": family.CommonDir})
			for _, g := range family.Graphs {
				rows["dedicated_graphs"] = append(rows["dedicated_graphs"], map[string]any{"graph_id": g.GraphID, "family_id": family.FamilyID, "owner_checkout_id": g.OwnerCheckoutID, "repo_prefix": g.RepoPrefix, "is_primary_base": 1})
				rows["tracking_intents"] = append(rows["tracking_intents"], map[string]any{"checkout_id": g.OwnerCheckoutID, "active": 1, "source_kind": "manual_config"})
			}
			for _, c := range family.Checkouts {
				rows["checkouts"] = append(rows["checkouts"], map[string]any{"checkout_id": c.CheckoutID, "incarnation": c.Incarnation, "family_id": family.FamilyID, "root_path": c.RootPath})
			}
		}
	}
	tables := []map[string]any{}
	for _, name := range []string{"repository_families", "checkouts", "tracking_intents", "dedicated_graphs"} {
		columns := []string{}
		for key := range rows[name][0] {
			columns = append(columns, key)
		}
		sort.Strings(columns)
		tables = append(tables, map[string]any{"name": name, "columns": columns, "rows": len(rows[name]), "sha256": strings.Repeat("a", 64), "catalog_rows": rows[name]})
	}
	tables = append(tables, map[string]any{"name": "nodes", "columns": []string{"id"}, "rows": 2, "sha256": strings.Repeat("b", 64)})
	observation := map[string]any{"schema_version": 44, "query_only": 1, "sqlite_version": "synthetic-unit", "collations": []string{"gortex_path_exact", "gortex_path_folded"}, "quick_check": []string{"ok"}, "integrity_check": []string{"ok"}, "schema_sha256": strings.Repeat("c", 64), "tables": tables}
	raw := map[string]any{"state": "NORMALIZED_OWNED_COPY_LOGICALLY_EQUIVALENT", "mode": "rw-copy-only-normal-checkpoint", "before": cloneNormalizationMap(t, observation), "after_checkpoint": cloneNormalizationMap(t, observation), "after_close": cloneNormalizationMap(t, observation), "checkpoint_busy_log_checkpointed": []int{0, 0, 0}, "input_header": 21, "final_header": 44, "final_main_sha256": l.MainSHA256, "final_wal_sha256": "", "final_wal_present": false, "final_wal_bytes": 0, "final_shm_absent": true}
	files := map[string]any{}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(filepath.Join(root, "store.db") + suffix)
		require.NoError(t, err)
		files["store.db"+suffix] = map[string]any{"path": filepath.Join(root, "store.db") + suffix, "sha256": sourceSHA(suffix), "size": info.Size(), "device": 1, "inode": 2, "mtime_ns": 3, "mode": 384, "nlink": 1}
	}
	helperSHA, operatorSHA, binarySHA := strings.Repeat("4", 64), strings.Repeat("5", 64), strings.Repeat("6", 64)
	f := &normalizationFixture{options: o, corpora: corpora, lineage: l, docs: map[string]map[string]any{}, proof: map[string]any{
		"state": "OBSERVED_COHERENT_COPY_NORMALIZATION", "observed_at": stamp(end), "owned_root": root, "source_upgrade_owner_pid": l.OwnerPID, "normalizer_pid": pid, "normalizer_exit_code": 0, "normal_sql_close": true, "owners_exited": true,
		"source_main_sha256": source.MainSHA256, "source_wal_sha256": source.WALSHA256, "source_shm_sha256": sourceSHA("-shm"), "snapshot": l.Snapshot, "main_sha256": l.MainSHA256, "wal_sha256": "", "wal_present": false, "shm_absent": true, "schema_version": 44, "input_raw_main_header": 21, "output_raw_main_header": 44,
		"checkpoint_busy_log_checkpointed": []int{0, 0, 0}, "logical_table_count": len(tables), "all_logical_snapshots_equal": true, "catalog_identity_count": len(expected), "operator_source_sha256": operatorSHA, "normalizer_binary_sha256": binarySHA, "registrar_source_commit": l.UpgradeSourceCommit, "helper_main_source_sha256": helperSHA, "helper_build_vcs": "disabled (-buildvcs=false), synthetic private helper", "qualification": "synthetic metadata/parser fixture only, not upgrade acceptance",
	}}
	f.docs["native"] = raw
	f.docs["plan"] = map[string]any{"source_root": root, "output_root": filepath.Dir(copyRoot), "files": files, "expected_schema": 44, "expected_raw_header_schema": 21, "native": map[string]any{"path": "not-executed-native", "sha256": binarySHA}, "operator": map[string]any{"path": "not-executed-operator", "sha256": operatorSHA}}
	f.docs["build"] = map[string]any{"source_commit": l.UpgradeSourceCommit, "source_tree": strings.Repeat("7", 40), "module_inputs": []map[string]any{{"path": "not-opened/main.go", "sha256": helperSHA}}, "product": map[string]any{"path": "not-executed-native", "sha256": binarySHA}, "vcs_qualification": "external private module -buildvcs=false"}
	f.docs["lane"] = map[string]any{"state": "terminal", "supervisor_pid": pid + 1, "child_pid": pid + 2, "exit": 0, "started_unix": float64(begin.UnixNano()) / 1e9, "finished_unix": float64(end.UnixNano()) / 1e9}
	f.docs["process"] = map[string]any{"pid": pid, "pgid": pid}
	f.docs["stop"] = map[string]any{"forced_or_failed": false, "normal_stop": true, "owners_exited": true, "pid": l.OwnerPID, "socket_absent": true}
	f.docs["identities"] = map[string]any{"by_root": expected}
	f.docs["composite"] = map[string]any{"product": map[string]any{"source_commit": l.UpgradeSourceCommit, "sha256": l.UpgradeBinarySHA256}, "original_failure_preserved": true}
	f.docs["copy_input"] = map[string]any{"state": "FRESH_COHERENT_OWNED_COPY_FOR_NORMALIZATION", "copy_root": copyRoot, "files": files}
	f.docs["copy_receipt"] = map[string]any{"state": "OWNED_COHERENT_TRIPLET_COPY", "copy_root": copyRoot, "files": files, "source_unchanged": true, "raw_main_header_schema": 21}
	f.docs["start"] = map[string]any{"state": "RUNNING_FRESH_COPY_NORMALIZATION", "at": stamp(begin), "operator_sha256": operatorSHA}
	f.docs["terminal"] = map[string]any{"state": "NORMALIZED_COPY_PROVENANCE_STEP_PASS", "error": nil, "source_triplet_unchanged": true, "at": stamp(end), "copy_after": map[string]any{"store.db": map[string]any{"exists": true, "sha256": l.MainSHA256}, "store.db-wal": map[string]any{"exists": false, "sha256": nil}, "store.db-shm": map[string]any{"exists": false, "sha256": nil}}}
	f.docs["exit"] = map[string]any{"state": "ACTUAL_NORMALIZATION_TERMINAL_CLEAN_FREE", "actual_start_unix": float64(begin.UnixNano()) / 1e9, "actual_finished_unix": float64(end.UnixNano()) / 1e9, "terminal_exit": 0, "supervisor_pid": pid + 1, "operator_pid": pid + 2, "native_pid": pid, "all_pids_absent": true, "all_recorded_groups_gone": true, "process_groups_checked": []int{pid + 1, pid + 2, pid}, "pending_absent": true, "outer_wrapper_exit": 0}
	f.options.Discovery.RuntimeRoot = discoveryRuntimeDestination(t, o)
	return f
}

func (f *normalizationFixture) write(t *testing.T) map[string]any {
	t.Helper()
	root := f.lineage.OwnedRoot
	refs := map[string]map[string]any{}
	write := func(name string, value any) map[string]any {
		path := filepath.Join(root, "normalization", name+".json")
		_ = os.Remove(path)
		require.NoError(t, discoveryWriteJSON(path, value))
		sha, err := discoveryFileSHA(path)
		require.NoError(t, err)
		return map[string]any{"path": path, "sha256": sha}
	}
	for _, name := range []string{"native", "plan", "build", "lane", "process", "stop", "identities", "composite", "copy_input", "copy_receipt"} {
		refs[name] = write(name, f.docs[name])
	}
	startLane := cloneNormalizationMap(t, f.docs["lane"])
	startLane["state"] = "running"
	delete(startLane, "exit")
	delete(startLane, "finished_unix")
	f.docs["start"]["admission"] = map[string]any{"plan": refs["plan"], "lane": startLane}
	refs["start"] = write("start", f.docs["start"])
	f.docs["terminal"]["native"] = f.docs["native"]
	f.docs["terminal"]["admission"] = map[string]any{"plan": refs["plan"], "lane": startLane}
	refs["terminal"] = write("terminal", f.docs["terminal"])
	for key, name := range map[string]string{"frozen_plan": "plan", "operator": "operator", "actual_terminal": "terminal", "native_stdout": "native", "native_process_receipt": "process", "copy_input": "copy_input", "copy_receipt": "copy_receipt"} {
		if name == "operator" {
			f.docs["exit"][key] = f.docs["plan"]["operator"]
		} else {
			f.docs["exit"][key] = refs[name]
		}
	}
	refs["exit"] = write("exit", f.docs["exit"])
	for key, name := range map[string]string{"native_observation": "native", "normalization_plan": "plan", "native_build_receipt": "build", "actual_lane_exit": "lane", "native_process": "process", "source_upgrade_stop_observation": "stop", "observed_catalog_identities": "identities", "composite_provenance": "composite", "actual_start": "start", "normalization_terminal": "terminal", "normalization_exit_receipt": "exit"} {
		f.proof[key] = refs[name]
	}
	f.proof["source_upgrade_stop"] = map[string]any{"path": f.lineage.StopReceipt, "sha256": f.lineage.StopReceiptSHA256}
	f.proof["source_upgrade_identity"] = map[string]any{"path": f.lineage.IdentityReceipt, "sha256": f.lineage.IdentityReceiptSHA256}
	var stop discoverySeedProof
	require.NoError(t, discoveryReadJSON(f.lineage.StopReceipt, &stop))
	f.proof["catalog_receipt"] = map[string]any{"path": stop.CatalogReceipt, "sha256": stop.CatalogReceiptSHA256}
	ref := write("normalization-proof", f.proof)
	raw, err := json.Marshal(f.lineage)
	require.NoError(t, err)
	var lineage map[string]any
	require.NoError(t, json.Unmarshal(raw, &lineage))
	lineage["normalization_proof"] = ref
	_ = os.Remove(f.options.Discovery.Resume)
	require.NoError(t, discoveryWriteJSON(f.options.Discovery.Resume, lineage))
	return lineage
}

func TestDiscoveryNormalizationPreservesDistinctDaemonAndNativeRoles(t *testing.T) {
	f := syntheticNormalization(t)
	f.write(t)
	before := discoverySourceHashes(t, f.lineage.OwnedRoot)
	p, err := resumeDiscoveryState(t.Context(), f.options, f.corpora)
	require.NoError(t, err, "original daemon stop must not be relabeled as normalized output")
	t.Cleanup(func() { _ = os.RemoveAll(p.SocketDir) })
	require.NoError(t, p.removeState())
	require.Equal(t, before, discoverySourceHashes(t, f.lineage.OwnedRoot))
	var stop discoverySeedProof
	require.NoError(t, discoveryReadJSON(f.lineage.StopReceipt, &stop))
	require.NotEqual(t, f.lineage.MainSHA256, stop.MainSHA256)
	require.Equal(t, f.lineage.OwnerPID, stop.OwnerPID)
}

func TestDiscoveryNormalizationCannotUseLegacySamePairAssertion(t *testing.T) {
	f := syntheticNormalization(t)
	require.NoError(t, discoveryWriteJSON(f.options.Discovery.Resume, f.lineage))
	_, err := resumeDiscoveryState(t.Context(), f.options, f.corpora)
	require.ErrorContains(t, err, "producer proof")
	require.NoDirExists(t, f.options.Discovery.RuntimeRoot)
}

func TestDiscoveryNormalizationRejectsContradictoryRawProvenance(t *testing.T) {
	for _, name := range []string{"roles_alias", "normal_close_missing", "native_exit_nonzero", "native_exit_missing", "source_pair", "operator_pin", "build_source_pin", "native_logical_change", "schema_change", "catalog_rekey", "missing_catalog_rows", "missing_count", "duplicate_table", "missing_after_close", "failed_integrity", "source_stop_not_normal", "exit_owner", "start_after_finish", "live_normalizer", "snapshot_shm", "snapshot_header"} {
		t.Run(name, func(t *testing.T) {
			f := syntheticNormalization(t)
			phase := func(which string) map[string]any { return f.docs["native"][which].(map[string]any) }
			table := func(which string, index int) map[string]any {
				return phase(which)["tables"].([]any)[index].(map[string]any)
			}
			switch name {
			case "roles_alias":
				f.proof["normalizer_pid"] = f.lineage.OwnerPID
			case "normal_close_missing":
				delete(f.proof, "normal_sql_close")
			case "native_exit_nonzero":
				f.proof["normalizer_exit_code"] = 1
			case "native_exit_missing":
				delete(f.proof, "normalizer_exit_code")
			case "source_pair":
				f.proof["source_main_sha256"] = strings.Repeat("f", 64)
			case "operator_pin":
				f.docs["plan"]["operator"].(map[string]any)["sha256"] = strings.Repeat("e", 64)
			case "build_source_pin":
				f.docs["build"]["source_commit"] = strings.Repeat("e", 40)
			case "native_logical_change":
				table("after_checkpoint", 4)["sha256"] = strings.Repeat("e", 64)
			case "schema_change":
				phase("after_close")["schema_sha256"] = strings.Repeat("e", 64)
			case "catalog_rekey":
				for _, which := range []string{"before", "after_checkpoint", "after_close"} {
					table(which, 1)["catalog_rows"].([]any)[0].(map[string]any)["incarnation"] = "synthetic-rekey"
				}
			case "missing_catalog_rows":
				delete(table("before", 0), "catalog_rows")
			case "missing_count":
				delete(table("before", 4), "rows")
			case "duplicate_table":
				table("before", 0)["name"] = "nodes"
			case "missing_after_close":
				delete(f.docs["native"], "after_close")
			case "failed_integrity":
				phase("after_close")["integrity_check"] = []string{"row missing"}
			case "source_stop_not_normal":
				f.docs["stop"]["normal_stop"] = false
			case "exit_owner":
				f.docs["exit"]["native_pid"] = f.lineage.OwnerPID
			case "start_after_finish":
				f.docs["start"]["at"] = time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339Nano)
			case "live_normalizer":
				pid := os.Getpid()
				f.proof["normalizer_pid"] = pid
				f.docs["process"]["pid"] = pid
				f.docs["process"]["pgid"] = pid
				f.docs["exit"]["native_pid"] = pid
				f.docs["exit"]["process_groups_checked"] = []int{pid}
			case "snapshot_shm":
				require.NoError(t, os.WriteFile(f.lineage.Snapshot+"-shm", nil, 0600))
			case "snapshot_header":
				bytes, err := os.ReadFile(f.lineage.Snapshot)
				require.NoError(t, err)
				binary.BigEndian.PutUint32(bytes[60:64], 21)
				require.NoError(t, os.WriteFile(f.lineage.Snapshot, bytes, 0600))
				sha, err := discoveryFileSHA(f.lineage.Snapshot)
				require.NoError(t, err)
				f.lineage.MainSHA256 = sha
				f.proof["main_sha256"] = sha
				f.docs["native"]["final_main_sha256"] = sha
				f.docs["terminal"]["copy_after"].(map[string]any)["store.db"].(map[string]any)["sha256"] = sha
			}
			f.write(t)
			before := discoverySourceHashes(t, f.lineage.OwnedRoot)
			p, err := resumeDiscoveryState(t.Context(), f.options, f.corpora)
			if p != nil {
				t.Cleanup(func() { _ = os.RemoveAll(p.SocketDir) })
			}
			require.Error(t, err, "summary assertions cannot replace raw role/hash/native evidence")
			require.NoDirExists(t, f.options.Discovery.RuntimeRoot)
			require.Equal(t, before, discoverySourceHashes(t, f.lineage.OwnedRoot))
		})
	}
}

func TestDiscoveryNormalizationRefCannotBeOpaqueOrRehashedWrongInput(t *testing.T) {
	for _, name := range []string{"opaque", "empty", "tampered_native_ref", "outside_owner", "source_shm"} {
		t.Run(name, func(t *testing.T) {
			f := syntheticNormalization(t)
			lineage := f.write(t)
			switch name {
			case "opaque":
				lineage["normalization_proof"] = map[string]any{"equivalent": true}
			case "empty":
				lineage["normalization_proof"] = map[string]any{}
			case "tampered_native_ref":
				f.proof["native_observation"].(map[string]any)["sha256"] = strings.Repeat("0", 64)
				ref := lineage["normalization_proof"].(map[string]any)
				path := ref["path"].(string)
				require.NoError(t, os.Remove(path))
				require.NoError(t, discoveryWriteJSON(path, f.proof))
				sha, err := discoveryFileSHA(path)
				require.NoError(t, err)
				ref["sha256"] = sha
			case "outside_owner":
				path := filepath.Join(t.TempDir(), "proof.json")
				require.NoError(t, discoveryWriteJSON(path, f.proof))
				sha, err := discoveryFileSHA(path)
				require.NoError(t, err)
				lineage["normalization_proof"] = map[string]any{"path": path, "sha256": sha}
			case "source_shm":
				f.proof["source_shm_sha256"] = strings.Repeat("e", 64)
				lineage = f.write(t)
			}
			require.NoError(t, os.Remove(f.options.Discovery.Resume))
			require.NoError(t, discoveryWriteJSON(f.options.Discovery.Resume, lineage))
			_, err := resumeDiscoveryState(t.Context(), f.options, f.corpora)
			require.Error(t, err)
			require.NoDirExists(t, f.options.Discovery.RuntimeRoot)
		})
	}
}

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"
)

type discoveryNormalizationRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type discoveryNormalizationProof struct {
	State                    string                    `json:"state"`
	ObservedAt               time.Time                 `json:"observed_at"`
	OwnedRoot                string                    `json:"owned_root"`
	SourceUpgradeOwnerPID    int                       `json:"source_upgrade_owner_pid"`
	NormalizerPID            int                       `json:"normalizer_pid"`
	NormalizerExitCode       *int                      `json:"normalizer_exit_code"`
	NormalSQLClose           bool                      `json:"normal_sql_close"`
	OwnersExited             bool                      `json:"owners_exited"`
	SourceMainSHA256         string                    `json:"source_main_sha256"`
	SourceWALSHA256          string                    `json:"source_wal_sha256"`
	SourceSHMSHA256          string                    `json:"source_shm_sha256"`
	Snapshot                 string                    `json:"snapshot"`
	MainSHA256               string                    `json:"main_sha256"`
	WALSHA256                string                    `json:"wal_sha256"`
	WALPresent               *bool                     `json:"wal_present"`
	SHMAbsent                bool                      `json:"shm_absent"`
	SchemaVersion            int                       `json:"schema_version"`
	InputHeader              int                       `json:"input_raw_main_header"`
	OutputHeader             int                       `json:"output_raw_main_header"`
	Checkpoint               []int                     `json:"checkpoint_busy_log_checkpointed"`
	LogicalTableCount        int                       `json:"logical_table_count"`
	AllLogicalSnapshotsEqual bool                      `json:"all_logical_snapshots_equal"`
	CatalogIdentityCount     int                       `json:"catalog_identity_count"`
	SourceStop               discoveryNormalizationRef `json:"source_upgrade_stop"`
	SourceIdentity           discoveryNormalizationRef `json:"source_upgrade_identity"`
	Catalog                  discoveryNormalizationRef `json:"catalog_receipt"`
	Native                   discoveryNormalizationRef `json:"native_observation"`
	Terminal                 discoveryNormalizationRef `json:"normalization_terminal"`
	Exit                     discoveryNormalizationRef `json:"normalization_exit_receipt"`
	Composite                discoveryNormalizationRef `json:"composite_provenance"`
	Build                    discoveryNormalizationRef `json:"native_build_receipt"`
	Plan                     discoveryNormalizationRef `json:"normalization_plan"`
	OperatorSHA256           string                    `json:"operator_source_sha256"`
	BinarySHA256             string                    `json:"normalizer_binary_sha256"`
	Start                    discoveryNormalizationRef `json:"actual_start"`
	LaneExit                 discoveryNormalizationRef `json:"actual_lane_exit"`
	Process                  discoveryNormalizationRef `json:"native_process"`
	StopObservation          discoveryNormalizationRef `json:"source_upgrade_stop_observation"`
	Identities               discoveryNormalizationRef `json:"observed_catalog_identities"`
	RegistrarCommit          string                    `json:"registrar_source_commit"`
	HelperSourceSHA256       string                    `json:"helper_main_source_sha256"`
	HelperBuildVCS           string                    `json:"helper_build_vcs"`
	Qualification            string                    `json:"qualification"`
}

type discoveryNormalizationTable struct {
	Name        string                       `json:"name"`
	Columns     []string                     `json:"columns"`
	Rows        *int64                       `json:"rows"`
	SHA256      string                       `json:"sha256"`
	CatalogRows []map[string]json.RawMessage `json:"catalog_rows,omitempty"`
}
type discoveryNormalizationObservation struct {
	SchemaVersion *int                          `json:"schema_version"`
	QueryOnly     *int                          `json:"query_only"`
	SQLiteVersion string                        `json:"sqlite_version"`
	Collations    []string                      `json:"collations"`
	Quick         []string                      `json:"quick_check"`
	Integrity     []string                      `json:"integrity_check"`
	SchemaSHA256  string                        `json:"schema_sha256"`
	Tables        []discoveryNormalizationTable `json:"tables"`
}
type discoveryNormalizationNative struct {
	State           string                            `json:"state"`
	Mode            string                            `json:"mode"`
	Before          discoveryNormalizationObservation `json:"before"`
	AfterCheckpoint discoveryNormalizationObservation `json:"after_checkpoint"`
	AfterClose      discoveryNormalizationObservation `json:"after_close"`
	Checkpoint      []int                             `json:"checkpoint_busy_log_checkpointed"`
	InputHeader     int                               `json:"input_header"`
	FinalHeader     int                               `json:"final_header"`
	FinalMainSHA256 string                            `json:"final_main_sha256"`
	FinalWALSHA256  string                            `json:"final_wal_sha256"`
	FinalWALPresent *bool                             `json:"final_wal_present"`
	FinalWALBytes   *int64                            `json:"final_wal_bytes"`
	FinalSHMAbsent  bool                              `json:"final_shm_absent"`
}

// Read authenticated JSON bytes once. Only the new public proof has a closed
// schema; raw producer observations may carry extra diagnostics/policy fields,
// which confer no authority and are never executed or treated as A-policy.
func readDiscoveryNormalizationRef(root string, ref discoveryNormalizationRef, out any, strict bool) error {
	if !discoveryDigestValid(ref.SHA256, 32) {
		return errors.New("normalization reference lacks a digest")
	}
	canonical, err := canonicalPath(ref.Path)
	if err != nil || canonical != ref.Path || root == ref.Path || !inside(root, ref.Path) {
		return errors.New("normalization reference is not canonical inside its source owner")
	}
	info, err := os.Lstat(ref.Path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return errors.New("normalization reference must be bounded regular JSON")
	}
	f, err := os.Open(ref.Path)
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	err = errors.Join(readErr, f.Close())
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	if len(data) > 4<<20 || hex.EncodeToString(digest[:]) != ref.SHA256 {
		return errors.New("normalization reference digest/size mismatch")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("normalization reference must contain one JSON value")
	}
	return nil
}

// Legacy upgrade owner/normal_stop keep their original meaning. A separate
// native actor may normalize a coherent COPY, never masquerade as daemon stop.
func readDiscoveryNormalization(root string, l discoverySeedLineage) (*discoveryNormalizationProof, error) {
	if l.NormalizationProof == nil {
		return nil, nil
	}
	var n discoveryNormalizationProof
	if err := readDiscoveryNormalizationRef(root, *l.NormalizationProof, &n, true); err != nil {
		return nil, err
	}
	if n.State != "OBSERVED_COHERENT_COPY_NORMALIZATION" || n.ObservedAt.IsZero() || n.OwnedRoot != root || n.SourceUpgradeOwnerPID != l.OwnerPID || n.NormalizerPID < 1 || n.NormalizerPID == l.OwnerPID || n.NormalizerExitCode == nil || *n.NormalizerExitCode != 0 || !n.NormalSQLClose || !n.OwnersExited {
		return nil, errors.New("normalization lacks distinct observed daemon/native roles and normal close/exit")
	}
	if n.Snapshot != l.Snapshot || n.MainSHA256 != l.MainSHA256 || n.WALSHA256 != l.WALSHA256 || n.WALPresent == nil || *n.WALPresent || n.WALSHA256 != "" || !n.SHMAbsent || n.SchemaVersion != 44 || n.InputHeader < 1 || n.InputHeader > 44 || n.OutputHeader != 44 || !slices.Equal(n.Checkpoint, []int{0, 0, 0}) || !n.AllLogicalSnapshotsEqual {
		return nil, errors.New("normalization does not bind the checkpointed output snapshot")
	}
	for _, digest := range []string{n.SourceMainSHA256, n.SourceWALSHA256, n.SourceSHMSHA256, n.MainSHA256, n.OperatorSHA256, n.BinarySHA256, n.HelperSourceSHA256} {
		if !discoveryDigestValid(digest, 32) {
			return nil, errors.New("normalization lacks source/output/build hash pins")
		}
	}
	if !discoveryDigestValid(n.RegistrarCommit, 20) || n.HelperBuildVCS == "" || n.SourceStop != (discoveryNormalizationRef{l.StopReceipt, l.StopReceiptSHA256}) || n.SourceIdentity != (discoveryNormalizationRef{l.IdentityReceipt, l.IdentityReceiptSHA256}) {
		return nil, errors.New("normalization source proof/pin mismatch")
	}
	return &n, nil
}

func normalizationTables(o discoveryNormalizationObservation) (map[string]discoveryNormalizationTable, error) {
	if o.SchemaVersion == nil || *o.SchemaVersion != 44 || o.QueryOnly == nil || *o.QueryOnly != 1 || o.SQLiteVersion == "" || !discoveryDigestValid(o.SchemaSHA256, 32) || !slices.Equal(o.Quick, []string{"ok"}) || !slices.Equal(o.Integrity, []string{"ok"}) || !slices.Contains(o.Collations, "gortex_path_exact") || !slices.Contains(o.Collations, "gortex_path_folded") {
		return nil, errors.New("normalization native schema/integrity/collation observation missing")
	}
	out := make(map[string]discoveryNormalizationTable, len(o.Tables))
	for _, table := range o.Tables {
		if _, duplicate := out[table.Name]; duplicate || table.Name == "" || len(table.Columns) == 0 || table.Rows == nil || *table.Rows < 0 || !discoveryDigestValid(table.SHA256, 32) {
			return nil, errors.New("normalization logical table inventory incomplete")
		}
		columns := map[string]bool{}
		for _, column := range table.Columns {
			if column == "" || columns[column] {
				return nil, errors.New("normalization table columns incomplete")
			}
			columns[column] = true
		}
		out[table.Name] = table
	}
	for _, name := range []string{"repository_families", "checkouts", "dedicated_graphs", "tracking_intents"} {
		t, ok := out[name]
		if !ok || int64(len(t.CatalogRows)) != *t.Rows {
			return nil, errors.New("normalization requires complete native catalog rows")
		}
	}
	return out, nil
}

func validateDiscoveryNormalization(root string, l discoverySeedLineage, n *discoveryNormalizationProof, catalog map[string]discoveryCatalogIdentity) error {
	if n == nil {
		return nil
	}
	var native discoveryNormalizationNative
	if err := readDiscoveryNormalizationRef(root, n.Native, &native, false); err != nil {
		return err
	}
	if native.State != "NORMALIZED_OWNED_COPY_LOGICALLY_EQUIVALENT" || native.Mode != "rw-copy-only-normal-checkpoint" || native.InputHeader != n.InputHeader || native.FinalHeader != 44 || native.FinalMainSHA256 != l.MainSHA256 || native.FinalWALSHA256 != "" || native.FinalWALPresent == nil || *native.FinalWALPresent || native.FinalWALBytes == nil || *native.FinalWALBytes != 0 || !native.FinalSHMAbsent || !slices.Equal(native.Checkpoint, n.Checkpoint) {
		return errors.New("normalization raw native output/checkpoint mismatch")
	}
	before, err := normalizationTables(native.Before)
	if err != nil {
		return err
	}
	if len(before) != n.LogicalTableCount {
		return errors.New("normalization logical table count mismatch")
	}
	for _, phase := range []discoveryNormalizationObservation{native.AfterCheckpoint, native.AfterClose} {
		after, err := normalizationTables(phase)
		if err != nil {
			return err
		}
		if phase.SchemaSHA256 != native.Before.SchemaSHA256 || phase.SQLiteVersion != native.Before.SQLiteVersion || !reflect.DeepEqual(before, after) {
			return errors.New("normalization logical observations changed across checkpoint/close")
		}
	}
	observed, err := normalizationCatalog(before)
	if err != nil {
		return err
	}
	if len(observed) != n.CatalogIdentityCount || !reflect.DeepEqual(observed, catalog) {
		return errors.New("normalization native catalog differs from upgrade identities")
	}
	var recorded struct {
		ByRoot map[string]discoveryCatalogIdentity `json:"by_root"`
	}
	if err := readDiscoveryNormalizationRef(root, n.Identities, &recorded, false); err != nil {
		return err
	}
	if !reflect.DeepEqual(recorded.ByRoot, catalog) {
		return errors.New("normalization recorded catalog differs from upgrade identities")
	}
	// Reuse the same public catalog parser and owner linkage checks as legacy.
	proof := discoverySeedProof{CatalogReceipt: n.Catalog.Path, CatalogReceiptSHA256: n.Catalog.SHA256}
	parsed, err := discoveryReadCatalogReceipt(root, proof, l.Repos)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(parsed, catalog) {
		return errors.New("normalization catalog proof differs")
	}
	return validateNormalizationProvenance(root, l, n, &native)
}

func normalizationText(row map[string]json.RawMessage, key string) (string, error) {
	var out string
	if err := json.Unmarshal(row[key], &out); err != nil || out == "" {
		return "", fmt.Errorf("normalization catalog missing %s", key)
	}
	return out, nil
}
func normalizationInteger(row map[string]json.RawMessage, key string) (int64, error) {
	var out *int64
	if err := json.Unmarshal(row[key], &out); err != nil || out == nil {
		return 0, fmt.Errorf("normalization catalog missing %s", key)
	}
	return *out, nil
}

func normalizationCatalog(tables map[string]discoveryNormalizationTable) (map[string]discoveryCatalogIdentity, error) {
	families := map[string]string{}
	for _, row := range tables["repository_families"].CatalogRows {
		id, e := normalizationText(row, "family_id")
		common, c := normalizationText(row, "common_dir_identity")
		if e != nil || c != nil || families[id] != "" {
			return nil, errors.New("normalization native family identity incomplete")
		}
		families[id] = common
	}
	type graphIdentity struct{ family, prefix, owner string }
	graphs, primary := map[string]graphIdentity{}, map[string]graphIdentity{}
	graphIDs := map[string]bool{}
	for _, row := range tables["dedicated_graphs"].CatalogRows {
		id, a := normalizationText(row, "graph_id")
		family, b := normalizationText(row, "family_id")
		owner, c := normalizationText(row, "owner_checkout_id")
		prefix, d := normalizationText(row, "repo_prefix")
		isPrimary, e := normalizationInteger(row, "is_primary_base")
		if a != nil || b != nil || c != nil || d != nil || e != nil || families[family] == "" || graphIDs[id] || graphs[owner].owner != "" || isPrimary < 0 || isPrimary > 1 {
			return nil, errors.New("normalization native graph owner identity incomplete")
		}
		item := graphIdentity{family, prefix, owner}
		graphs[owner] = item
		graphIDs[id] = true
		if isPrimary == 1 {
			if primary[family].owner != "" {
				return nil, errors.New("normalization duplicate primary graph")
			}
			primary[family] = item
		}
	}
	if len(primary) != len(families) {
		return nil, errors.New("normalization primary graph missing")
	}
	out := map[string]discoveryCatalogIdentity{}
	ids := map[string]string{}
	for _, row := range tables["checkouts"].CatalogRows {
		id, a := normalizationText(row, "checkout_id")
		inc, b := normalizationText(row, "incarnation")
		family, c := normalizationText(row, "family_id")
		path, d := normalizationText(row, "root_path")
		prefix := primary[family].prefix
		if graph, ok := graphs[id]; ok {
			if graph.family != family {
				return nil, errors.New("normalization graph family mismatch")
			}
			prefix = graph.prefix
		}
		if a != nil || b != nil || c != nil || d != nil || prefix == "" || families[family] == "" || ids[id] != "" || out[path].Root != "" {
			return nil, errors.New("normalization checkout identity incomplete")
		}
		out[path] = discoveryCatalogIdentity{Root: path, CheckoutID: id, Incarnation: inc, FamilyID: family, RepoPrefix: prefix, CommonDir: families[family]}
		ids[id] = path
	}
	for owner, graph := range graphs {
		path := ids[owner]
		if path == "" || out[path].FamilyID != graph.family {
			return nil, errors.New("normalization graph owner missing")
		}
	}
	for _, row := range tables["tracking_intents"].CatalogRows {
		owner, a := normalizationText(row, "checkout_id")
		active, b := normalizationInteger(row, "active")
		if a != nil || b != nil || ids[owner] == "" || active < 0 || active > 1 {
			return nil, errors.New("normalization intent owner/active state invalid")
		}
		if active == 1 {
			if _, err := normalizationText(row, "source_kind"); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

type discoveryNormalizationSourceFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Mtime  int64  `json:"mtime_ns"`
	Mode   uint32 `json:"mode"`
	Nlink  uint64 `json:"nlink"`
}
type discoveryNormalizationLane struct {
	State      string  `json:"state"`
	Supervisor int     `json:"supervisor_pid"`
	Child      int     `json:"child_pid"`
	Start      float64 `json:"started_unix"`
	Finished   float64 `json:"finished_unix"`
	Exit       *int    `json:"exit"`
}
type discoveryNormalizationAdmission struct {
	Plan discoveryNormalizationRef  `json:"plan"`
	Lane discoveryNormalizationLane `json:"lane"`
}

func validateNormalizationProvenance(root string, l discoverySeedLineage, n *discoveryNormalizationProof, native *discoveryNormalizationNative) error {
	read := func(ref discoveryNormalizationRef, out any) error {
		return readDiscoveryNormalizationRef(root, ref, out, false)
	}
	var plan struct {
		SourceRoot  string                                      `json:"source_root"`
		OutputRoot  string                                      `json:"output_root"`
		Files       map[string]discoveryNormalizationSourceFile `json:"files"`
		Native      discoveryNormalizationRef                   `json:"native"`
		Operator    discoveryNormalizationRef                   `json:"operator"`
		Schema      int                                         `json:"expected_schema"`
		InputHeader int                                         `json:"expected_raw_header_schema"`
	}
	if err := read(n.Plan, &plan); err != nil {
		return err
	}
	if plan.SourceRoot != root || plan.OutputRoot != filepath.Dir(filepath.Dir(l.Snapshot)) || plan.Schema != 44 || plan.InputHeader != n.InputHeader || plan.Native.SHA256 != n.BinarySHA256 || plan.Operator.SHA256 != n.OperatorSHA256 {
		return errors.New("normalization plan source/output/build pins differ")
	}
	var build struct {
		SourceCommit string                      `json:"source_commit"`
		SourceTree   string                      `json:"source_tree"`
		Inputs       []discoveryNormalizationRef `json:"module_inputs"`
		Product      discoveryNormalizationRef   `json:"product"`
	}
	if err := read(n.Build, &build); err != nil {
		return err
	}
	foundSource := false
	for _, input := range build.Inputs {
		if filepath.Base(input.Path) == "main.go" && input.SHA256 == n.HelperSourceSHA256 {
			foundSource = true
		}
	}
	if build.SourceCommit != n.RegistrarCommit || !discoveryDigestValid(build.SourceTree, 20) || build.Product.SHA256 != n.BinarySHA256 || !foundSource {
		return errors.New("normalization native build/source pins differ")
	}
	// Registrar commit is an imported dependency, never an embedded helper VCS
	// assertion. No source/binary paths from build or plan are opened/executed.
	var process struct {
		PID  int `json:"pid"`
		PGID int `json:"pgid"`
	}
	if err := read(n.Process, &process); err != nil {
		return err
	}
	if process.PID != n.NormalizerPID || process.PGID < 1 {
		return errors.New("normalization native process identity differs")
	}
	var stop struct {
		Forced       *bool `json:"forced_or_failed"`
		Normal       bool  `json:"normal_stop"`
		Exited       bool  `json:"owners_exited"`
		PID          int   `json:"pid"`
		SocketAbsent bool  `json:"socket_absent"`
	}
	if err := read(n.StopObservation, &stop); err != nil {
		return err
	}
	if stop.Forced == nil || *stop.Forced || !stop.Normal || !stop.Exited || !stop.SocketAbsent || stop.PID != l.OwnerPID {
		return errors.New("normalization must retain the original daemon stop observation")
	}
	var composite struct {
		Product struct {
			Source string `json:"source_commit"`
			SHA    string `json:"sha256"`
		} `json:"product"`
	}
	if err := read(n.Composite, &composite); err != nil {
		return err
	}
	if composite.Product.Source != l.UpgradeSourceCommit || composite.Product.SHA != l.UpgradeBinarySHA256 {
		return errors.New("normalization upgrade product provenance differs")
	}
	var lane discoveryNormalizationLane
	if err := read(n.LaneExit, &lane); err != nil {
		return err
	}
	var start struct {
		State       string                          `json:"state"`
		At          time.Time                       `json:"at"`
		OperatorSHA string                          `json:"operator_sha256"`
		Admission   discoveryNormalizationAdmission `json:"admission"`
	}
	if err := read(n.Start, &start); err != nil {
		return err
	}
	var exit struct {
		NativePID     int                       `json:"native_pid"`
		Supervisor    int                       `json:"supervisor_pid"`
		OperatorPID   int                       `json:"operator_pid"`
		Exit          *int                      `json:"terminal_exit"`
		OuterExit     *int                      `json:"outer_wrapper_exit"`
		Start         float64                   `json:"actual_start_unix"`
		Finished      float64                   `json:"actual_finished_unix"`
		PIDsAbsent    bool                      `json:"all_pids_absent"`
		GroupsGone    bool                      `json:"all_recorded_groups_gone"`
		Groups        []int                     `json:"process_groups_checked"`
		PendingAbsent bool                      `json:"pending_absent"`
		Plan          discoveryNormalizationRef `json:"frozen_plan"`
		Operator      discoveryNormalizationRef `json:"operator"`
		Terminal      discoveryNormalizationRef `json:"actual_terminal"`
		Native        discoveryNormalizationRef `json:"native_stdout"`
		Process       discoveryNormalizationRef `json:"native_process_receipt"`
		CopyInput     discoveryNormalizationRef `json:"copy_input"`
		CopyReceipt   discoveryNormalizationRef `json:"copy_receipt"`
	}
	if err := read(n.Exit, &exit); err != nil {
		return err
	}
	if exit.NativePID != n.NormalizerPID || exit.Supervisor < 1 || exit.OperatorPID < 1 || exit.Exit == nil || *exit.Exit != 0 || exit.OuterExit == nil || *exit.OuterExit != 0 || !exit.PIDsAbsent || !exit.GroupsGone || !exit.PendingAbsent || !slices.Contains(exit.Groups, process.PGID) || exit.Plan.SHA256 != n.Plan.SHA256 || exit.Operator.SHA256 != n.OperatorSHA256 || exit.Terminal != n.Terminal || exit.Native != n.Native || exit.Process != n.Process {
		return errors.New("normalization actual exit/ref chain is incomplete")
	}
	observed := float64(n.ObservedAt.UnixNano()) / 1e9
	started := float64(start.At.UnixNano()) / 1e9
	if start.State != "RUNNING_FRESH_COPY_NORMALIZATION" || start.At.IsZero() || start.OperatorSHA != n.OperatorSHA256 || start.Admission.Plan.SHA256 != n.Plan.SHA256 || start.Admission.Lane.State != "running" || start.Admission.Lane.Supervisor != exit.Supervisor || start.Admission.Lane.Child != exit.OperatorPID || start.Admission.Lane.Start != exit.Start || lane.State != "terminal" || lane.Exit == nil || *lane.Exit != 0 || lane.Supervisor != exit.Supervisor || lane.Child != exit.OperatorPID || lane.Start != exit.Start || lane.Finished != exit.Finished || exit.Start <= 0 || exit.Finished < exit.Start || started < exit.Start || started > observed || observed > exit.Finished {
		return errors.New("normalization actual start/exit chronology or ownership differs")
	}
	var terminal struct {
		State           string                          `json:"state"`
		Error           json.RawMessage                 `json:"error"`
		Native          discoveryNormalizationNative    `json:"native"`
		Admission       discoveryNormalizationAdmission `json:"admission"`
		SourceUnchanged bool                            `json:"source_triplet_unchanged"`
		At              time.Time                       `json:"at"`
		CopyAfter       map[string]struct {
			Exists *bool   `json:"exists"`
			SHA    *string `json:"sha256"`
		} `json:"copy_after"`
	}
	if err := read(n.Terminal, &terminal); err != nil {
		return err
	}
	if terminal.State != "NORMALIZED_COPY_PROVENANCE_STEP_PASS" || !bytes.Equal(terminal.Error, []byte("null")) || !terminal.SourceUnchanged || !terminal.At.Equal(n.ObservedAt) || !reflect.DeepEqual(terminal.Native, *native) || terminal.Admission.Plan.SHA256 != n.Plan.SHA256 || terminal.Admission.Lane.Supervisor != exit.Supervisor || terminal.Admission.Lane.Child != exit.OperatorPID {
		return errors.New("normalization terminal does not bind the raw native observations")
	}
	main := terminal.CopyAfter["store.db"]
	wal := terminal.CopyAfter["store.db-wal"]
	shm := terminal.CopyAfter["store.db-shm"]
	if main.Exists == nil || !*main.Exists || main.SHA == nil || *main.SHA != l.MainSHA256 || wal.Exists == nil || *wal.Exists || wal.SHA != nil || shm.Exists == nil || *shm.Exists || shm.SHA != nil {
		return errors.New("normalization terminal output pair/SHM differs")
	}
	var input struct {
		State string                                      `json:"state"`
		Root  string                                      `json:"copy_root"`
		Files map[string]discoveryNormalizationSourceFile `json:"files"`
	}
	var copied struct {
		State           string                                      `json:"state"`
		Root            string                                      `json:"copy_root"`
		Files           map[string]discoveryNormalizationSourceFile `json:"files"`
		SourceUnchanged bool                                        `json:"source_unchanged"`
		InputHeader     int                                         `json:"raw_main_header_schema"`
	}
	if err := read(exit.CopyInput, &input); err != nil {
		return err
	}
	if err := read(exit.CopyReceipt, &copied); err != nil {
		return err
	}
	if input.State != "FRESH_COHERENT_OWNED_COPY_FOR_NORMALIZATION" || copied.State != "OWNED_COHERENT_TRIPLET_COPY" || input.Root != filepath.Dir(l.Snapshot) || copied.Root != input.Root || !copied.SourceUnchanged || copied.InputHeader != n.InputHeader || !reflect.DeepEqual(input.Files, copied.Files) || !reflect.DeepEqual(input.Files, plan.Files) || len(input.Files) != 3 {
		return errors.New("normalization coherent input copy observations differ")
	}
	for suffix, want := range map[string]string{"": n.SourceMainSHA256, "-wal": n.SourceWALSHA256, "-shm": n.SourceSHMSHA256} {
		file := input.Files["store.db"+suffix]
		if file.Path != filepath.Join(root, "store.db")+suffix || file.SHA256 != want || file.Size <= 0 {
			return errors.New("normalization original input hashes differ from upgrade")
		}
	}
	return nil
}

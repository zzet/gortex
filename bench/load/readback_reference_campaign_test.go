//go:build acceptance

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

var readbackReferencePlanPath = flag.String("readback-reference-plan", "", "explicit frozen readback plan; omission never launches a daemon")
var readbackReferencePhase = flag.String("readback-reference-phase", "preflight", "preflight, protocol_preflight, no_edit, under_edit, compare or main_compare")
var readbackReferenceCellIndex = flag.Int("readback-reference-cell", -1, "exact frozen cell index; no implicit next cell")
var readbackReferenceRefusalEvidence = flag.Bool("readback-reference-refusal-evidence", false, "explicit diagnostic-only forwarding of GORTEX_SEARCH_REFUSAL_EVIDENCE=1; schedules and strict first responses unchanged")
var readbackReferenceExecute = flag.Bool("readback-reference-execute", false, "execute one explicitly granted reference cell")
var readbackReferenceBaselineReport = flag.String("readback-reference-baseline-report", "", "exact no-edit artifact for offline paired comparison")
var readbackReferenceMainReport = flag.String("readback-reference-main-report", "", "existing load read-only main report")
var readbackReferenceCandidateReadReport = flag.String("readback-reference-candidate-read-report", "", "same-driver candidate read-only report")
var readbackReferenceMainRevision = flag.String("readback-reference-main-revision", "", "full frozen main binary revision for read-only comparison")

type readbackReferenceCaseReport struct {
	ManifestSHA256          string                              `json:"manifest_sha256"`
	ComparisonIdentity      readbackReferenceComparisonIdentity `json:"comparison_identity"`
	ArrivalIdentityReason   string                              `json:"arrival_identity_reason,omitempty"`
	Timings                 map[string]Distribution             `json:"timings_ms"`
	RuntimeEnvironment      map[string]string                   `json:"runtime_environment"`
	Version                 int                                 `json:"version"`
	PlanSHA256              string                              `json:"plan_sha256"`
	Plan                    readbackReferencePlan               `json:"plan"`
	Cell                    readbackReferenceCell               `json:"cell"`
	Role                    string                              `json:"role"`
	Host                    Host                                `json:"host"`
	HostBefore              readbackReferenceHostSnapshot       `json:"host_before"`
	HostAfter               readbackReferenceHostSnapshot       `json:"host_after"`
	ExpectedOffers          int                                 `json:"expected_offers"`
	ObservedOffers          int                                 `json:"observed_offers"`
	AcceptanceNominalOffers int                                 `json:"acceptance_nominal_offers"`
	OwnedRoot               string                              `json:"owned_root"`
	SQLite                  string                              `json:"sqlite_path"`
	NativeRows              int                                 `json:"native_fixture_rows_checked_before_phase"`
	ProbeCWD                string                              `json:"probe_cwd"`
	Prefix                  string                              `json:"repo_prefix"`
	BootConfig              json.RawMessage                     `json:"owned_boot_config"`
	Readiness               map[string]json.RawMessage          `json:"readiness"`
	DaemonPID               int                                 `json:"daemon_pid"`
	Started                 time.Time                           `json:"started"`
	Finished                time.Time                           `json:"finished"`
	FixturePopulationSHA256 string                              `json:"after_image_population_sha256"`
	LogicalReadPlanSHA256   string                              `json:"logical_read_plan_sha256"`
	Records                 []readbackReferenceRecord           `json:"records"`
	Correctness             readbackReferenceObservation        `json:"correctness"`
	Cleanup                 map[string]any                      `json:"cleanup"`
	Errors                  []string                            `json:"errors"`
	AcceptanceState         string                              `json:"acceptance_state"`
	RefusalEvidenceOptIn    bool                                `json:"refusal_evidence_requested,omitempty"`
	ProtocolOnly            bool                                `json:"protocol_only"`
	ProtocolPass            bool                                `json:"protocol_pass"`
	ToolSurface             string                              `json:"tool_surface"`
}

func (report *readbackReferenceCaseReport) retainHostAfter(after readbackReferenceHostSnapshot) error {
	report.HostAfter = after // Keep failed/unavailable snapshots, too.
	return compareReadbackReferenceHosts(report.HostBefore, after)
}

func readbackReferenceWriteJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	e := json.NewEncoder(f)
	e.SetIndent("", "  ")
	return errors.Join(e.Encode(value), f.Sync(), f.Close())
}
func TestReadbackReferenceCampaign(t *testing.T) {
	if *readbackReferencePlanPath == "" {
		t.Skip("no frozen operator plan: no runtime launched")
	}
	raw, err := os.ReadFile(*readbackReferencePlanPath)
	if err != nil {
		t.Fatal(err)
	}
	p, err := decodeReadbackReferencePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	preflight, err := preflightReadbackReference(p)
	if err != nil {
		t.Fatal(err)
	}
	if *readbackReferencePhase == "preflight" {
		if err := readbackReferenceWriteJSON(p.Output+".preflight.json", preflight); err != nil {
			t.Fatal(err)
		}
		t.Log("preflight only: no daemon, store or workload launched")
		return
	}
	if *readbackReferencePhase == "compare" {
		if *readbackReferenceCellIndex < 0 || *readbackReferenceCellIndex >= len(preflight.Cells) {
			t.Fatal("select one exact frozen cell for comparison")
		}
		underPath := fmt.Sprintf("%s.cell-%03d.under_edit.json", p.Output, *readbackReferenceCellIndex)
		var under, base readbackReferenceCaseReport
		for path, target := range map[string]*readbackReferenceCaseReport{underPath: &under, *readbackReferenceBaselineReport: &base} {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, target); err != nil {
				t.Fatal(err)
			}
		}
		errorsByEndpoint := map[string]string{}
		pass := under.Role == "under_edit" && base.Role == "no_edit" && under.Correctness.Pass && base.Correctness.Pass && len(under.Errors) == 0 && len(base.Errors) == 0
		for _, name := range []string{"source", "summary", "editing_context", "file", "symbol"} {
			err := compareReadbackReferenceTiming(name, under.Cell.Agents, under.Timings[name], base.Timings[name], under.ComparisonIdentity, base.ComparisonIdentity)
			if err != nil {
				pass = false
				errorsByEndpoint[name] = err.Error()
			}
		}
		verdict := map[string]any{"paired_timing_pass": pass, "under_edit": underPath, "no_edit": *readbackReferenceBaselineReport, "errors": errorsByEndpoint, "global_acceptance": "unavailable: remaining matrix and separate main read-only comparison required"}
		if err := readbackReferenceWriteJSON(p.Output+fmt.Sprintf(".cell-%03d.compare.json", *readbackReferenceCellIndex), verdict); err != nil {
			t.Fatal(err)
		}
		if !pass {
			t.Fatal("paired timing/correctness comparison failed or unavailable")
		}
		return
	}
	if *readbackReferencePhase == "main_compare" {
		main, err := readReport(*readbackReferenceMainReport)
		if err != nil {
			t.Fatal(err)
		}
		candidate, err := readReport(*readbackReferenceCandidateReadReport)
		if err != nil {
			t.Fatal(err)
		}
		comparisonErr := compareReadbackReferenceMain(main, candidate, *readbackReferenceMainRevision, p.BinaryRevision)
		verdict := map[string]any{"main_read_only_pass": comparisonErr == nil, "agents": candidate.Workload.Agents, "main": *readbackReferenceMainReport, "candidate": *readbackReferenceCandidateReadReport, "qualification": "separate main no-edit mix only; not an under-edit reference matrix verdict"}
		if comparisonErr != nil {
			verdict["error"] = comparisonErr.Error()
		}
		if err := readbackReferenceWriteJSON(p.Output+fmt.Sprintf(".main-compare-%d.json", candidate.Workload.Agents), verdict); err != nil {
			t.Fatal(err)
		}
		if comparisonErr != nil {
			t.Fatal(comparisonErr)
		}
		return
	}
	if !*readbackReferenceExecute || p.GrantID == "" {
		t.Fatal("one explicit A grant and execute flag are required; no runtime launched")
	}
	if *readbackReferenceCellIndex < 0 || *readbackReferenceCellIndex >= len(preflight.Cells) {
		t.Fatal("select exactly one frozen cell")
	}
	role := *readbackReferencePhase
	cell := preflight.Cells[*readbackReferenceCellIndex]
	if role != "under_edit" && role != "no_edit" && role != "protocol_preflight" {
		t.Fatal("unknown runtime role")
	}
	if role == "protocol_preflight" {
		cell = readbackReferenceProtocolSchedule(cell, time.Duration(p.ProductDeadlineSeconds)*time.Second)
	}
	if role == "no_edit" && cell.Cell.ACKMode != "" {
		t.Fatal("no-edit representative uses default full; local reuse requires proven equivalent report")
	}
	output := fmt.Sprintf("%s.cell-%03d.%s.json", p.Output, *readbackReferenceCellIndex, role)
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused existing or unavailable output identity")
	}
	report, runErr := runReadbackReferenceCase(t.Context(), p, cell, role)
	if err := readbackReferenceWriteJSON(output, report); err != nil {
		t.Fatal(errors.Join(runErr, err))
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
}

func runReadbackReferenceCase(parent context.Context, plan readbackReferencePlan, schedule readbackReferenceSchedule, role string) (report readbackReferenceCaseReport, resultErr error) {
	report = readbackReferenceCaseReport{Version: 1, Plan: plan, Cell: schedule.Cell, Role: role, ExpectedOffers: readbackReferencePhaseOffered(schedule, role), AcceptanceNominalOffers: schedule.Cell.Offers, Readiness: map[string]json.RawMessage{}, Cleanup: map[string]any{}, Errors: []string{}, AcceptanceState: "UNAVAILABLE: one cell only; matched no-edit/main comparison and remaining required matrix are not certified"}
	surface, err := readbackReferenceSurface(plan.ToolSurface)
	if err != nil {
		return report, err
	}
	report.ToolSurface = surface
	report.RefusalEvidenceOptIn = *readbackReferenceRefusalEvidence
	operationRole := role
	hostProfile := "reference"
	if role == "protocol_preflight" {
		report.ProtocolOnly = true
		report.AcceptanceState = "UNAVAILABLE: one diagnostic offer; not an acceptance sample, pacing or latency verdict"
		operationRole = "under_edit"
		hostProfile = "native"
	}
	report.PlanSHA256, _ = hashJSON(plan)
	report.ManifestSHA256, resultErr = readbackReferenceFileSHA(plan.Manifest)
	if resultErr != nil {
		return report, resultErr
	}
	report.RuntimeEnvironment = map[string]string{}
	for _, key := range []string{"GOMAXPROCS", "GOMEMLIMIT", "GOGC", "GODEBUG"} {
		report.RuntimeEnvironment[key] = os.Getenv(key)
	}
	defer func() {
		report.Finished = time.Now()
		if resultErr != nil {
			report.Errors = append(report.Errors, resultErr.Error())
		}
	}()
	hostBefore, err := inspectReadbackReferenceHost(hostProfile, plan.Scratch)
	report.HostBefore, report.Host = hostBefore, hostBefore.Host
	if err != nil {
		return report, err
	}
	defer func() {
		// Reuse the same existing probe directory after the fresh child path is
		// allocated/removed. Persist both snapshots even on an error or mismatch.
		after, inspectErr := inspectReadbackReferenceHost(hostProfile, hostBefore.ProbePath)
		identityErr := report.retainHostAfter(after)
		if inspectErr != nil || identityErr != nil {
			report.ProtocolPass = false
			resultErr = errors.Join(resultErr, inspectErr, identityErr)
		}
	}()
	corpora, err := loadManifest(plan.Manifest)
	if err != nil {
		return report, err
	}
	prepareCtx, prepareCancel := context.WithTimeout(parent, time.Duration(plan.WarmSeconds)*time.Second)
	p, err := newPrivateRun(prepareCtx, plan.Scratch, plan.Binary, corpora)
	defer prepareCancel()
	if err != nil {
		return report, err
	}
	report.OwnedRoot, report.SQLite = p.Root, p.Store
	var clients []*socketClient
	defer func() {
		for _, client := range clients {
			resultErr = errors.Join(resultErr, client.Close())
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Duration(plan.StopSeconds)*time.Second)
		_, forced, stopErr := p.stop(stopCtx)
		cancel()
		resultErr = errors.Join(resultErr, stopErr)
		report.Cleanup["forced"] = forced
		report.Cleanup["stop_error"] = ""
		if stopErr != nil {
			report.Cleanup["stop_error"] = stopErr.Error()
		}
		resultErr = errors.Join(resultErr, os.RemoveAll(p.SocketDir), p.removeState())
		_, socketErr := os.Lstat(p.Socket)
		report.Cleanup["socket_absent"] = errors.Is(socketErr, os.ErrNotExist)
		_, storeErr := os.Lstat(p.Store)
		report.Cleanup["owned_store_removed"] = errors.Is(storeErr, os.ErrNotExist)
		report.Cleanup["native_handle_cleared"] = p.process == nil
		report.Cleanup["native_leader_joined"] = false
		select {
		case <-p.done:
			report.Cleanup["native_leader_joined"] = true
		default:
		}
		report.Cleanup["native_group_closure"] = "unavailable: outer supervisor must observe owned process-group disappearance"
	}()
	repo := p.Repos[0]
	fixtureRoot := filepath.Join(repo.Path, "zz_gortex_readback_probe")
	if err := os.Mkdir(fixtureRoot, 0700); err != nil {
		return report, fmt.Errorf("fixture namespace must be freshly absent: %w", err)
	}
	trials := make([]readbackReferenceTrial, len(schedule.Arrival))
	population := map[string]string{}
	var readPlan []map[string]request
	for i, arrival := range schedule.Arrival {
		trial, err := readbackReferenceTrialFor(schedule.Cell, arrival, i)
		if err != nil {
			return report, err
		}
		trials[i] = trial
		readPlan = append(readPlan, readbackReferenceProbeRequests(trial))
		images := trial.After
		if operationRole == "under_edit" {
			images = trial.Before
		}
		for file, image := range images {
			absolute := filepath.Join(repo.Path, filepath.FromSlash(file))
			if !inside(fixtureRoot, absolute) {
				return report, errors.New("generated fixture escaped owned namespace")
			}
			if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
				return report, err
			}
			if err := os.WriteFile(absolute, []byte(image.Bytes), 0600); err != nil {
				return report, err
			}
		}
		for file, image := range trial.After {
			population[file] = readbackReferenceSHA(image.Bytes)
		}
	}
	report.FixturePopulationSHA256, _ = hashJSON(population)
	report.LogicalReadPlanSHA256, _ = hashJSON(readPlan)
	if _, err := os.Lstat(p.Store); !errors.Is(err, os.ErrNotExist) {
		return report, errors.New("private SQLite store was not absent before daemon start")
	}
	p.editACKMode = schedule.Cell.ACKMode
	configured, err := p.writeBootRepos(false)
	if err != nil {
		return report, err
	}
	report.BootConfig, err = os.ReadFile(filepath.Join(p.Root, "config", "gortex", "config.yaml"))
	if err != nil {
		return report, err
	}
	started := time.Now()
	if err := p.startReadbackReferenceDaemon(role, *readbackReferenceRefusalEvidence); err != nil {
		return report, err
	}
	report.DaemonPID = p.process.Process.Pid
	warmCtx, warmCancel := prepareCtx, prepareCancel
	defer warmCancel()
	if err := p.awaitSocket(warmCtx); err != nil {
		return report, err
	}
	gate, _, err := p.awaitReady(warmCtx, started, configured)
	report.Readiness = gate
	if err != nil {
		return report, err
	}
	var ready readiness
	if err := json.Unmarshal(gate["readiness"], &ready); err != nil {
		return report, err
	}
	for _, tracked := range ready.TrackedRepos {
		if filepath.Clean(tracked.Path) == filepath.Clean(repo.Path) {
			repo.Prefix = tracked.Prefix
		}
	}
	if repo.Prefix == "" {
		return report, errors.New("owning primary prefix missing from readiness")
	}
	p.Repos[0] = repo
	report.Prefix, report.ProbeCWD = repo.Prefix, repo.Path
	for range schedule.Cell.Agents {
		var client *socketClient
		var err error
		if surface == readbackReferenceCompatibilitySurface {
			params, parameterErr := readbackReferenceCompatibilityParams(trials[0].Request, repo)
			if parameterErr != nil {
				return report, parameterErr
			}
			var availability map[string]json.RawMessage
			client, availability, err = openReadbackReferenceCompatibility(warmCtx, p.Socket, repo.Path, params["name"].(string))
			for tool, raw := range availability {
				report.Readiness[fmt.Sprintf("compatibility.editor-%d.%s", len(clients), tool)] = raw
			}
		} else {
			client, err = openMCP(warmCtx, p.Socket, repo.Path)
		}
		if err != nil {
			return report, err
		}
		clients = append(clients, client)
	}
	var analysis map[string]json.RawMessage
	if surface == readbackReferenceCompatibilitySurface {
		analysis, err = warmReadbackReferenceCompatibility(warmCtx, clients[0], time.Duration(plan.ProductDeadlineSeconds)*time.Second)
	} else {
		analysis, _, err = warmAnalysis(warmCtx, clients[0], time.Duration(plan.ProductDeadlineSeconds)*time.Second)
	}
	for k, v := range analysis {
		report.Readiness[k] = v
	}
	if err != nil {
		return report, err
	}
	report.NativeRows, err = checkReadbackReferenceSQLite(warmCtx, p.Store, repo.Prefix, trials, operationRole)
	if err != nil {
		return report, err
	}
	warmCancel()
	report.Records = make([]readbackReferenceRecord, len(trials))
	for i, trial := range trials {
		report.Records[i] = readbackReferenceRecord{Ordinal: i, Editor: trial.Editor, DueNS: trial.DueNS, CWD: repo.Path, Probes: map[string]readbackReferenceRPC{}, Errors: []string{"phase censored before dispatch"}}
	}
	var projectionBefore *projectionRuntimeBookend
	if *readbackProjectionEvidence {
		value := collectProjectionBookend(parent, clients[0], report.DaemonPID, "before", nil)
		retainProjectionBookend(&report, value)
		projectionBefore = &value
	}
	report.Started = time.Now()
	phaseCtx, phaseCancel := context.WithTimeout(parent, time.Duration(schedule.PhaseBoundNS))
	defer phaseCancel()
	runReadbackReferenceOffers(phaseCtx, report.Started, trials, func(i int, trial readbackReferenceTrial) {
		actual := time.Now()
		var row readbackReferenceRecord
		if surface == readbackReferenceCompatibilitySurface {
			row = observeReadbackReferenceCompatibility(phaseCtx, clients[trial.Editor], repo, trial, operationRole, time.Duration(plan.ProductDeadlineSeconds)*time.Second)
		} else {
			row = observeReadbackReference(phaseCtx, clients[trial.Editor], repo, trial, operationRole, time.Duration(plan.ProductDeadlineSeconds)*time.Second)
		}
		row.StartLatenessNS = actual.Sub(report.Started.Add(time.Duration(trial.DueNS))).Nanoseconds()
		row.PhysicalBytesCorrect = true
		for file, image := range trial.After {
			actualPath, err := ownedFile(repo.Path, file)
			if err != nil {
				row.PhysicalBytesCorrect = false
				row.Errors = append(row.Errors, err.Error())
				continue
			}
			f, err := os.Open(actualPath)
			if err != nil {
				row.PhysicalBytesCorrect = false
				row.Errors = append(row.Errors, err.Error())
				continue
			}
			content, readErr := io.ReadAll(io.LimitReader(f, int64(len(image.Bytes)+1)))
			closeErr := f.Close()
			if readErr != nil || closeErr != nil || string(content) != image.Bytes {
				row.PhysicalBytesCorrect = false
				row.Errors = append(row.Errors, "post-probe disk bytes differ from planned after-image")
			}
		}
		expectedMode := "full"
		if schedule.Cell.ACKMode != "" {
			expectedMode = schedule.Cell.ACKMode
		}
		if operationRole == "under_edit" && row.ObservedACKMode != "" && row.ObservedACKMode != expectedMode {
			row.Errors = append(row.Errors, "observed ACK mode contradicts configured policy")
		}
		row.Correct = row.Correct && row.PhysicalBytesCorrect && len(row.Errors) == 0
		report.Records[i] = row
	})
	if *readbackProjectionEvidence {
		retainProjectionBookend(&report, collectProjectionBookend(parent, clients[0], report.DaemonPID, "after", projectionBefore))
	}
	correct := make([]bool, len(report.Records))
	for i, row := range report.Records {
		correct[i] = row.Correct
	}
	report.ObservedOffers = readbackReferenceObservedOffers(report.Records)
	report.Correctness = summarizeReadbackReference(report.ExpectedOffers, correct)
	currentSHA, err := readbackReferenceFileSHA(plan.Binary)
	if err != nil || currentSHA != plan.BinarySHA256 {
		return report, errors.New("binary identity changed during runtime")
	}
	oldHostHash, hostErr := readbackReferenceHostIdentity(report.HostBefore)
	if hostErr != nil {
		return report, hostErr
	}
	currentManifest, manifestErr := readbackReferenceFileSHA(plan.Manifest)
	if manifestErr != nil || currentManifest != report.ManifestSHA256 {
		return report, errors.New("manifest identity changed during phase")
	}
	report.ComparisonIdentity = readbackReferenceComparisonIdentity{BinarySHA256: plan.BinarySHA256, ManifestSHA256: report.ManifestSHA256, HostSHA256: oldHostHash, PopulationSHA256: report.FixturePopulationSHA256, ReadPlanSHA256: report.LogicalReadPlanSHA256, Agents: schedule.Cell.Agents, Kind: schedule.Cell.Kind, ToolSurface: surface}
	report.ComparisonIdentity.ArrivalSHA256, err = readbackReferenceActualArrivalSHA(schedule, report.Records, operationRole, surface, report.Started)
	if err != nil {
		// Keep every censored/missing offer. Unknown execution cannot borrow a
		// seed-derived planned hash or grant baseline equivalence.
		report.ArrivalIdentityReason = err.Error()
	}
	report.Timings = map[string]Distribution{}
	for _, name := range []string{"source", "summary", "editing_context", "file", "symbol"} {
		report.Timings[name] = readbackReferenceTiming(report.Records, name, report.ExpectedOffers)
	}
	if report.ProtocolOnly {
		report.ProtocolPass = len(correct) == 1 && correct[0]
		if !report.ProtocolPass {
			return report, errors.New("one-off actual-protocol diagnostic failed; no acceptance credit")
		}
	} else if !report.Correctness.Pass {
		return report, errors.New("immediate exact readback below99.9%; all100 offers retained")
	}
	return report, nil
}

// Read-only census of the freshly created native SQLite store, outside timing.
// It cannot supply freshness credit for later public readbacks.
func checkReadbackReferenceSQLite(ctx context.Context, store, prefix string, trials []readbackReferenceTrial, role string) (int, error) {
	f, err := os.Open(store)
	if err != nil {
		return 0, err
	}
	header := make([]byte, 16)
	_, err = io.ReadFull(f, header)
	err = errors.Join(err, f.Close())
	if err != nil || string(header) != "SQLite format 3\x00" {
		return 0, errors.New("native backend is not a real SQLite store")
	}
	uri := url.URL{Scheme: "file", Path: store}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	uri.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return 0, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	defer db.Close()
	checked := 0
	for _, trial := range trials {
		images := trial.After
		if role == "under_edit" {
			images = trial.Before
		}
		files := make([]string, 0, len(images))
		for file := range images {
			files = append(files, file)
		}
		sort.Strings(files)
		for _, file := range files {
			image := images[file]
			rows, err := db.QueryContext(ctx, "SELECT DISTINCT id,start_line,end_line FROM nodes WHERE (file_path=? OR file_path=?) AND kind='function' LIMIT 128", file, prefix+"/"+file)
			if err != nil {
				return checked, err
			}
			seen := map[string]bool{}
			for rows.Next() {
				var id string
				var start, end int
				if err := rows.Scan(&id, &start, &end); err != nil {
					rows.Close()
					return checked, err
				}
				for name, symbol := range image.Symbols {
					if id == prefix+"/"+file+"::"+name && start == symbol.StartLine && end == symbol.EndLine {
						seen[name] = true
					}
				}
			}
			scanErr := rows.Err()
			closeErr := rows.Close()
			if scanErr != nil || closeErr != nil {
				return checked, errors.Join(scanErr, closeErr)
			}
			if len(seen) != len(image.Symbols) {
				return checked, fmt.Errorf("native SQLite missing initial fixture rows for %s", file)
			}
			checked += len(seen)
		}
	}
	return checked, nil
}

// The existing one-offer protocol diagnostic is intrinsically selected. Other
// existing runtime roles forward the flag only with the separate explicit SDK
// diagnostic switch; ordinary acceptance remains isolated by default.
func (p *privateRun) startReadbackReferenceDaemon(role string, explicit bool) error {
	selected := role == "protocol_preflight" || explicit && (role == "under_edit" || role == "no_edit")
	return p.startWithSearchRefusalEvidence(selected)
}

package main

// These names are a closed contract, not user-supplied JSON paths. The wire is
// bench/load report-v1 at #996 commit 95b7982bf1853b24bd963a0701aa163d9a72a2de.
// A declared target with no faithful observation remains a failing prerequisite.
type metricSpec struct {
	unit        string
	axis        string
	missing     string
	everyRepeat bool
}

var metricSpecs = map[string]metricSpec{
	"edit_ack_p95":          {unit: "s", axis: "timing"},
	"edit_ack_max":          {unit: "s", axis: "timing", everyRepeat: true},
	"read_p95":              {unit: "s", axis: "timing"},
	"read_p95_vs_load8":     {unit: "ratio", axis: "timing"},
	"read_p95_vs_main":      {unit: "ratio", axis: "timing"},
	"search_symbols_p95":    {unit: "s", axis: "timing"},
	"read_source_p99":       {unit: "s", axis: "timing"},
	"outline_p50":           {unit: "s", axis: "timing"},
	"dead_code_p50":         {unit: "s", axis: "timing"},
	"dead_code_vs_single":   {unit: "ratio", axis: "timing"},
	"busy_fraction":         {unit: "fraction", axis: "refusal"},
	"daemon_cpu_cores":      {unit: "cores", axis: "cpu"},
	"stop_seconds":          {unit: "s", axis: "timing", everyRepeat: true},
	"force_kills":           {unit: "count", axis: "safety", everyRepeat: true},
	"panics":                {unit: "count", axis: "safety", everyRepeat: true},
	"sigaborts":             {unit: "count", axis: "safety", everyRepeat: true},
	"oom_kills":             {unit: "count", axis: "safety", everyRepeat: true},
	"wal_peak_mib":          {unit: "MiB", axis: "disk"},
	"cold_written_mib":      {unit: "MiB", axis: "disk"},
	"idle_memory_2s_mib":    {unit: "MiB", axis: "memory"},
	"peak_memory_mib":       {unit: "MiB", axis: "memory"},
	"worktree_memory_mib":   {unit: "MiB", axis: "memory"},
	"edit_written_mib":      {unit: "MiB/edit", axis: "disk"},
	"readback_p95":          {unit: "s", axis: "timing", missing: "visible_latency_ms is indexed-symbol visibility, not edit plus exact file readback"},
	"readback_correct":      {unit: "fraction", axis: "safety", everyRepeat: true, missing: "report-v1 does not retain exact edited-file byte correctness probes"},
	"unflagged_stale":       {unit: "count", axis: "safety", everyRepeat: true, missing: "report-v1 has no stale-flag correctness census across output modes"},
	"relations_lag_p95":     {unit: "s", axis: "timing", missing: "indexed-symbol visibility does not measure relations publication"},
	"derived_lag_p95":       {unit: "s", axis: "timing", missing: "report-v1 has no derived-publication lag observation"},
	"reclaim_hold_p95":      {unit: "s", axis: "timing", missing: "report-v1 has no WAL-reclaim writer-hold distribution"},
	"read_gate_closures":    {unit: "count", axis: "safety", everyRepeat: true, missing: "report-v1 has no read-gate closure census"},
	"cancel_release_max":    {unit: "s", axis: "timing", everyRepeat: true, missing: "report-v1 does not measure cancellation-to-slot-release"},
	"impact_stale_refusals": {unit: "count", axis: "safety", everyRepeat: true, missing: "attempt code/error does not provide a structured stale-refusal reason census"},
	"detect_stale_refusals": {unit: "count", axis: "safety", everyRepeat: true, missing: "report-v1 lacks detect_changes primary-checkout refusal probes"},
	"repo_not_tracked":      {unit: "count", axis: "safety", everyRepeat: true, missing: "startup polling does not retain a complete configured-repo/worktree refusal census"},
	"first_ok_after_ready":  {unit: "s", axis: "timing", missing: "restart queryability measures launch-to-queryable, not ready-to-first-OK"},
	"idle_written_mib_min":  {unit: "MiB/min", axis: "disk", missing: "report-v1 lacks an idle-write observation interval"},
	"idle_memory_60s_mib":   {unit: "MiB", axis: "memory", missing: "idle_after_2s is not a 60-second post-enrichment footprint observation"},
	"cold_go_sys_peak_mib":  {unit: "MiB", axis: "memory", missing: "daemon status Go Sys snapshots do not establish cold-window peak Go Sys"},
}

type scenarioSpec struct {
	wire      string
	agents    int
	editors   int
	repos     int
	worktrees int
	missing   string
}

var scenarioSpecs = buildScenarioSpecs()

func buildScenarioSpecs() map[string]scenarioSpec {
	out := make(map[string]scenarioSpec)
	for _, cell := range []struct {
		name string
		n    int
	}{{"1", 1}, {"4", 4}, {"8", 8}, {"16", 16}, {"32", 32}} {
		out["load-"+cell.name] = scenarioSpec{wire: "read", agents: cell.n, repos: 1}
		out["loadedit-"+cell.name] = scenarioSpec{wire: "edit", editors: cell.n, repos: 1}
	}
	out["multirepoedit-6"] = scenarioSpec{wire: "many-edit", editors: 6, repos: 6}
	for _, cell := range []struct {
		name string
		n    int
	}{{"4", 4}, {"8", 8}, {"16", 16}} {
		out["worktrees-"+cell.name] = scenarioSpec{wire: "worktrees", agents: 4, repos: 1, worktrees: cell.n, missing: "frozen worktrees drives read bursts, not the required linked-worktree edit probes"}
	}
	out["cold-gortex"] = scenarioSpec{wire: "cold", agents: 1, repos: 1}
	out["warm-restart"] = scenarioSpec{wire: "restart", agents: 1, repos: 1}
	for _, name := range []string{"gortex", "corpus-1", "corpus-2", "corpus-3", "corpus-4", "corpus-5"} {
		out["tools-single-"+name] = scenarioSpec{wire: "sweep", agents: 1, repos: 1}
		out["tools-six-"+name] = scenarioSpec{wire: "sweep", agents: 1, repos: 6, missing: "frozen sweep loads manifest[0], not six scoped sessions"}
	}
	for _, name := range []string{"idle-stop", "idle-stop-20", "cancel", "churn-60", "cold-six", "schema21-upgrade", "cold-start", "large-freelist", "restart-831", "agent-e2e", "lapsed-upgrade"} {
		out[name] = scenarioSpec{missing: "frozen load CLI does not identify this lifecycle/fixture as a distinct measured scenario"}
	}
	return out
}

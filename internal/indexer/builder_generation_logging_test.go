package indexer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func newSparseBuildLoggingFixture(t testing.TB) (sparseBuildFlightFixture, *observer.ObservedLogs) {
	t.Helper()
	fixture := newSparseBuildFlightFixture(t)
	core, logs := observer.New(zap.DebugLevel)
	fixture.builder.Logger = zap.New(core)
	return fixture, logs
}

func sparseBuildTelemetryEntries(logs *observer.ObservedLogs) []observer.LoggedEntry {
	entries := make([]observer.LoggedEntry, 0)
	for _, entry := range logs.All() {
		switch entry.Message {
		case sparsePhysicalBuildStartedMessage,
			sparsePhysicalBuildCompletedMessage,
			sparsePhysicalBuildFailedMessage,
			sparseReadyReuseMessage,
			sparseFollowerReuseMessage:
			entries = append(entries, entry)
		}
	}
	return entries
}

func sparseBuildLogEntriesByMessage(
	t testing.TB,
	logs *observer.ObservedLogs,
	message string,
	want int,
) []observer.LoggedEntry {
	t.Helper()
	entries := logs.FilterMessage(message).All()
	if len(entries) != want {
		t.Fatalf("%q entries = %d, want %d", message, len(entries), want)
	}
	return entries
}

func sparseBuildLogField(t testing.TB, entry observer.LoggedEntry, key string) any {
	t.Helper()
	value, ok := entry.ContextMap()[key]
	if !ok {
		t.Fatalf("%q log has no %q field: %#v", entry.Message, key, entry.ContextMap())
	}
	return value
}

func sparseBuildLogInt64(t testing.TB, entry observer.LoggedEntry, key string) int64 {
	t.Helper()
	switch value := sparseBuildLogField(t, entry, key).(type) {
	case int:
		return int64(value)
	case int32:
		return int64(value)
	case int64:
		return value
	case time.Duration:
		return int64(value)
	case uint:
		return int64(value)
	case uint32:
		return int64(value)
	case uint64:
		return int64(value)
	case float64:
		return int64(value)
	default:
		t.Fatalf("%q field %q has non-integer type %T", entry.Message, key, value)
		return 0
	}
}

func sparseBuildLogBool(t testing.TB, entry observer.LoggedEntry, key string) bool {
	t.Helper()
	value, ok := sparseBuildLogField(t, entry, key).(bool)
	if !ok {
		t.Fatalf("%q field %q is %T, want bool", entry.Message, key, sparseBuildLogField(t, entry, key))
	}
	return value
}

func assertSparseBuildTelemetryPathFree(
	t testing.TB,
	entries []observer.LoggedEntry,
	secrets ...string,
) {
	t.Helper()
	for _, entry := range entries {
		lowerMessage := strings.ToLower(entry.Message)
		if strings.Contains(lowerMessage, ".go") || strings.Contains(entry.Message, "/") {
			t.Errorf("telemetry message contains a path: %q", entry.Message)
		}
		for _, secret := range secrets {
			if secret != "" && strings.Contains(entry.Message, secret) {
				t.Errorf("telemetry message contains secret path %q: %q", secret, entry.Message)
			}
		}
		for key, value := range entry.ContextMap() {
			lowerKey := strings.ToLower(key)
			if strings.Contains(lowerKey, "path") || strings.Contains(lowerKey, "root") || strings.Contains(lowerKey, "repo") {
				t.Errorf("telemetry field key %q can carry a path", key)
			}
			text, ok := value.(string)
			if !ok {
				continue
			}
			if strings.Contains(strings.ToLower(text), ".go") || strings.Contains(text, "/") {
				t.Errorf("telemetry field %q contains a path-like value %q", key, text)
			}
			for _, secret := range secrets {
				if secret != "" && strings.Contains(text, secret) {
					t.Errorf("telemetry field %q contains secret path %q", key, secret)
				}
			}
		}
	}
}

func TestSparseGenerationPhysicalBuildLogsOneLeaderAndCoalescedFollowers(t *testing.T) {
	fixture, logs := newSparseBuildLoggingFixture(t)
	const callers = 8
	entered := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	var physicalPasses atomic.Int64
	fixture.builder.beforePhysicalPass = func(int64) error {
		if physicalPasses.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil
	}

	start := make(chan struct{})
	results := make(chan sparseBuildFlightResult, callers)
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			generationID, report, err := fixture.builder.Build(context.Background(), fixture.request)
			results <- sparseBuildFlightResult{generationID: generationID, report: report, err: err}
		}()
	}
	close(start)
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("physical build did not reach its index pass")
	}
	generationID, _, adopted, err := fixture.store.BeginPayloadGenerationWithStatus(
		context.Background(), payloadRequestForBuild(fixture.request),
	)
	if err != nil || !adopted {
		t.Fatalf("observe building generation: adopted=%t err=%v", adopted, err)
	}
	awaitSparseBuildFlightWaiters(t, fixture.store, generationID, callers-1)
	close(release)
	released = true

	collected := make([]sparseBuildFlightResult, 0, callers)
	collected = append(collected, <-results)
	// Build returns only after its defer emits the terminal and wakes the
	// flight, so the first returned leader or follower must observe it.
	sparseBuildLogEntriesByMessage(t, logs, sparsePhysicalBuildCompletedMessage, 1)
	for len(collected) < callers {
		collected = append(collected, <-results)
	}

	var physicalReport BuildReport
	physical := 0
	coalesced := 0
	for i, result := range collected {
		if result.err != nil {
			t.Fatalf("build %d: %v", i, result.err)
		}
		if result.generationID != generationID {
			t.Errorf("build %d generation = %d, want %d", i, result.generationID, generationID)
		}
		if result.report.Coalesced {
			coalesced++
		} else {
			physical++
			physicalReport = result.report
		}
	}
	if physical != 1 || coalesced != callers-1 {
		t.Fatalf("physical=%d coalesced=%d, want 1 and %d", physical, coalesced, callers-1)
	}

	startEntry := sparseBuildLogEntriesByMessage(t, logs, sparsePhysicalBuildStartedMessage, 1)[0]
	completeEntry := sparseBuildLogEntriesByMessage(t, logs, sparsePhysicalBuildCompletedMessage, 1)[0]
	if startEntry.Level != zap.InfoLevel || completeEntry.Level != zap.InfoLevel {
		t.Fatalf("physical levels = %s/%s, want info/info", startEntry.Level, completeEntry.Level)
	}
	if got := len(logs.FilterMessage(sparsePhysicalBuildFailedMessage).All()); got != 0 {
		t.Fatalf("failed terminal entries = %d, want 0", got)
	}
	followers := sparseBuildLogEntriesByMessage(t, logs, sparseFollowerReuseMessage, callers-1)
	for _, entry := range followers {
		if entry.Level != zap.DebugLevel {
			t.Errorf("follower level = %s, want debug", entry.Level)
		}
		if !sparseBuildLogBool(t, entry, "coalesced") {
			t.Error("follower log does not identify coalescing")
		}
		if got := fmt.Sprint(sparseBuildLogField(t, entry, "reuse_kind")); got != "in_flight" {
			t.Errorf("follower reuse_kind = %q, want in_flight", got)
		}
		if !sparseBuildLogBool(t, entry, "success") {
			t.Error("successful follower was logged as failed")
		}
		if sparseBuildLogInt64(t, entry, "duration") <= 0 {
			t.Error("follower wait duration is not positive")
		}
	}

	if got := sparseBuildLogInt64(t, startEntry, "generation_id"); got != generationID {
		t.Errorf("start generation_id = %d, want %d", got, generationID)
	}
	if sparseBuildLogBool(t, startEntry, "coalesced") || sparseBuildLogBool(t, startEntry, "recovery") || sparseBuildLogBool(t, startEntry, "adopted") {
		t.Error("new physical leader was logged as coalesced or recovery")
	}
	startCounts := map[string]int64{
		"changed_files": int64(physicalReport.ChangedFiles),
		"added_files":   int64(physicalReport.AddedFiles),
		"deleted_files": int64(physicalReport.DeletedFiles),
		"closure_files": int64(physicalReport.ClosureFiles),
		"indexed_files": int64(len(physicalReport.IndexedPaths)),
		"source_bytes":  physicalReport.SourceBytes,
	}
	for key, want := range startCounts {
		if got := sparseBuildLogInt64(t, startEntry, key); got != want {
			t.Errorf("start %s = %d, want %d", key, got, want)
		}
	}
	if got := sparseBuildLogBool(t, startEntry, "closure_truncated"); got != physicalReport.ClosureTruncated {
		t.Errorf("start closure_truncated = %t, want %t", got, physicalReport.ClosureTruncated)
	}
	if sparseBuildLogInt64(t, startEntry, "planning_duration") <= 0 {
		t.Error("start planning_duration is not positive")
	}

	if got := sparseBuildLogInt64(t, completeEntry, "generation_id"); got != generationID {
		t.Errorf("completion generation_id = %d, want %d", got, generationID)
	}
	if sparseBuildLogBool(t, completeEntry, "coalesced") || !sparseBuildLogBool(t, completeEntry, "success") {
		t.Error("successful physical completion has wrong coalesced/success fields")
	}
	terminalCounts := map[string]int64{
		"node_count":          int64(physicalReport.NodeCount),
		"edge_count":          int64(physicalReport.EdgeCount),
		"replace_masks":       int64(physicalReport.ReplaceMasks),
		"delete_masks":        int64(physicalReport.DeleteMasks),
		"node_tombstones":     int64(physicalReport.NodeTombstones),
		"edge_source_markers": int64(physicalReport.EdgeSourceMarkers),
	}
	for key, want := range terminalCounts {
		if got := sparseBuildLogInt64(t, completeEntry, key); got != want {
			t.Errorf("completion %s = %d, want %d", key, got, want)
		}
	}
	if sparseBuildLogInt64(t, completeEntry, "duration") <= 0 {
		t.Error("completion duration is not positive")
	}

	telemetry := sparseBuildTelemetryEntries(logs)
	if len(telemetry) != callers+1 {
		t.Fatalf("telemetry entries = %d, want %d", len(telemetry), callers+1)
	}
	assertSparseBuildTelemetryPathFree(t, telemetry)
}

func TestSparseGenerationPhysicalBuildFailureLogDoesNotLeakPaths(t *testing.T) {
	fixture, logs := newSparseBuildLoggingFixture(t)
	secret := "/secret/worktree/core.go"
	fixture.request.PrePublish = func(context.Context, int64) error {
		return errors.New(secret)
	}

	_, _, err := fixture.builder.Build(context.Background(), fixture.request)
	if err == nil || !strings.Contains(err.Error(), secret) {
		t.Fatalf("build error = %v, want returned secret-bearing error", err)
	}
	sparseBuildLogEntriesByMessage(t, logs, sparsePhysicalBuildStartedMessage, 1)
	failed := sparseBuildLogEntriesByMessage(t, logs, sparsePhysicalBuildFailedMessage, 1)[0]
	if failed.Level != zap.ErrorLevel || sparseBuildLogBool(t, failed, "success") {
		t.Fatalf("failed terminal level/success = %s/%t, want error/false",
			failed.Level, sparseBuildLogBool(t, failed, "success"))
	}
	if sparseBuildLogInt64(t, failed, "duration") <= 0 {
		t.Error("failed terminal duration is not positive")
	}
	if got := len(logs.FilterMessage(sparsePhysicalBuildCompletedMessage).All()); got != 0 {
		t.Fatalf("successful terminal entries = %d, want 0", got)
	}
	assertSparseBuildTelemetryPathFree(t, sparseBuildTelemetryEntries(logs), secret)
}

func TestSparseGenerationPhysicalBuildCancellationLogsOneTerminal(t *testing.T) {
	fixture, logs := newSparseBuildLoggingFixture(t)
	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	fixture.builder.beforePhysicalPass = func(int64) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	result := make(chan error, 1)
	go func() {
		_, _, err := fixture.builder.Build(ctx, fixture.request)
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("physical build did not reach its index pass")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled build error = %v, want context canceled", err)
	}
	sparseBuildLogEntriesByMessage(t, logs, sparsePhysicalBuildStartedMessage, 1)
	failed := sparseBuildLogEntriesByMessage(t, logs, sparsePhysicalBuildFailedMessage, 1)[0]
	if failed.Level != zap.ErrorLevel || sparseBuildLogBool(t, failed, "success") {
		t.Fatalf("canceled terminal level/success = %s/%t, want error/false",
			failed.Level, sparseBuildLogBool(t, failed, "success"))
	}
	if got := len(logs.FilterMessage(sparsePhysicalBuildCompletedMessage).All()); got != 0 {
		t.Fatalf("successful terminal entries = %d, want 0", got)
	}
	assertSparseBuildTelemetryPathFree(t, sparseBuildTelemetryEntries(logs))
}

func TestSparseGenerationPhysicalBuildPanicLogsOneTerminal(t *testing.T) {
	fixture, logs := newSparseBuildLoggingFixture(t)
	secret := "/secret/worktree/panic.go"
	fixture.request.PrePublish = func(context.Context, int64) error {
		panic(secret)
	}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _, _ = fixture.builder.Build(context.Background(), fixture.request)
	}()
	if recovered != secret {
		t.Fatalf("recovered panic = %v, want %q", recovered, secret)
	}
	sparseBuildLogEntriesByMessage(t, logs, sparsePhysicalBuildStartedMessage, 1)
	failed := sparseBuildLogEntriesByMessage(t, logs, sparsePhysicalBuildFailedMessage, 1)[0]
	if failed.Level != zap.ErrorLevel || sparseBuildLogBool(t, failed, "success") {
		t.Fatalf("panic terminal level/success = %s/%t, want error/false",
			failed.Level, sparseBuildLogBool(t, failed, "success"))
	}
	if got := len(logs.FilterMessage(sparsePhysicalBuildCompletedMessage).All()); got != 0 {
		t.Fatalf("successful terminal entries = %d, want 0", got)
	}
	assertSparseBuildTelemetryPathFree(t, sparseBuildTelemetryEntries(logs), secret)
}

func TestSparseGenerationFollowerCancellationLogReportsWaitOutcome(t *testing.T) {
	fixture, logs := newSparseBuildLoggingFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	var physicalPasses atomic.Int64
	fixture.builder.beforePhysicalPass = func(int64) error {
		if physicalPasses.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil
	}

	leaderResult := make(chan sparseBuildFlightResult, 1)
	go func() {
		generationID, report, err := fixture.builder.Build(context.Background(), fixture.request)
		leaderResult <- sparseBuildFlightResult{generationID: generationID, report: report, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("physical build did not reach its index pass")
	}
	generationID, _, adopted, err := fixture.store.BeginPayloadGenerationWithStatus(
		context.Background(), payloadRequestForBuild(fixture.request),
	)
	if err != nil || !adopted {
		t.Fatalf("observe building generation: adopted=%t err=%v", adopted, err)
	}

	followerCtx, cancelFollower := context.WithCancel(context.Background())
	followerResult := make(chan sparseBuildFlightResult, 1)
	go func() {
		followerGenerationID, report, buildErr := fixture.builder.Build(followerCtx, fixture.request)
		followerResult <- sparseBuildFlightResult{
			generationID: followerGenerationID,
			report:       report,
			err:          buildErr,
		}
	}()
	awaitSparseBuildFlightWaiters(t, fixture.store, generationID, 1)
	cancelFollower()
	follower := <-followerResult
	if !errors.Is(follower.err, context.Canceled) {
		t.Fatalf("follower error = %v, want context canceled", follower.err)
	}
	if follower.generationID != generationID || !follower.report.Coalesced {
		t.Fatalf("follower generation/coalesced = %d/%t, want %d/true",
			follower.generationID, follower.report.Coalesced, generationID)
	}

	entry := sparseBuildLogEntriesByMessage(t, logs, sparseFollowerReuseMessage, 1)[0]
	if entry.Level != zap.DebugLevel || sparseBuildLogBool(t, entry, "success") {
		t.Fatalf("canceled follower level/success = %s/%t, want debug/false",
			entry.Level, sparseBuildLogBool(t, entry, "success"))
	}
	if got := sparseBuildLogInt64(t, entry, "generation_id"); got != generationID {
		t.Errorf("follower generation_id = %d, want %d", got, generationID)
	}
	if sparseBuildLogInt64(t, entry, "duration") <= 0 {
		t.Error("canceled follower wait duration is not positive")
	}
	if got := len(logs.FilterMessage(sparsePhysicalBuildCompletedMessage).All()); got != 0 {
		t.Fatalf("physical completion before leader release = %d, want 0", got)
	}
	if got := len(logs.FilterMessage(sparsePhysicalBuildFailedMessage).All()); got != 0 {
		t.Fatalf("physical failure before leader release = %d, want 0", got)
	}

	close(release)
	released = true
	leader := <-leaderResult
	if leader.err != nil {
		t.Fatalf("physical leader: %v", leader.err)
	}
	sparseBuildLogEntriesByMessage(t, logs, sparsePhysicalBuildCompletedMessage, 1)
	assertSparseBuildTelemetryPathFree(t, sparseBuildTelemetryEntries(logs))
}

func TestSparseGenerationReadyReuseLogIsDebugAndDistinct(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	builder := &SparseGenerationBuilder{Logger: zap.New(core)}
	const reuseDuration = 7 * time.Millisecond
	builder.logSparseBuildReuse(sparseReadyReuseMessage, 73, "ready", reuseDuration, false, true)

	entry := sparseBuildLogEntriesByMessage(t, logs, sparseReadyReuseMessage, 1)[0]
	if entry.Level != zap.DebugLevel {
		t.Fatalf("ready reuse level = %s, want debug", entry.Level)
	}
	if got := sparseBuildLogInt64(t, entry, "generation_id"); got != 73 {
		t.Errorf("ready reuse generation_id = %d, want 73", got)
	}
	if !sparseBuildLogBool(t, entry, "coalesced") {
		t.Error("ready reuse is not marked coalesced")
	}
	if got := fmt.Sprint(sparseBuildLogField(t, entry, "reuse_kind")); got != "ready" {
		t.Errorf("ready reuse kind = %q, want ready", got)
	}
	if got := sparseBuildLogInt64(t, entry, "duration"); got != int64(reuseDuration) {
		t.Errorf("ready reuse duration = %d, want %d", got, reuseDuration)
	}
	if _, ok := entry.ContextMap()["success"]; ok {
		t.Error("ready reuse log unexpectedly has a wait success field")
	}
	assertSparseBuildTelemetryPathFree(t, sparseBuildTelemetryEntries(logs))
}

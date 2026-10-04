package main

import (
	"encoding/json"
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on http.DefaultServeMux
	"os"
	"runtime"
	"strconv"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search"
)

// pprofAddr holds the bound address of the daemon's pprof listener
// (empty when pprof is disabled). Read by controller.Status so the
// active address is reported to clients; written exactly once from
// startPProfIfEnabled. atomic.Value guards against the data race that
// would otherwise exist between Status calls and startup.
var pprofAddr atomic.Value

func init() {
	pprofAddr.Store("")
}

// daemonPProfAddr returns the currently-bound pprof listener address,
// or an empty string when no listener is running.
func daemonPProfAddr() string {
	v, _ := pprofAddr.Load().(string)
	return v
}

// startPProfIfEnabled opens an HTTP pprof listener when the user has
// set GORTEX_DAEMON_PPROF_ADDR (e.g. "127.0.0.1:6060"). Opt-in by
// design — leaving a pprof endpoint on by default would expose the
// daemon's internal state to any process on the machine. The listener
// runs in its own goroutine; failures are logged but don't block
// daemon startup.
func startPProfIfEnabled(logger *zap.Logger) {
	addr := os.Getenv("GORTEX_DAEMON_PPROF_ADDR")
	if addr == "" {
		return
	}
	// pprof serves the DefaultServeMux with no auth, and /debug/pprof/cmdline
	// echoes the daemon's argv — which carries --http-auth-token. The heap it
	// dumps holds indexed source from every tracked repository. Keep it on
	// loopback: an operator who wants it reachable can port-forward.
	if !isLocalhostBind(addr) {
		logger.Warn("daemon: refusing non-loopback pprof bind",
			zap.String("addr", addr),
			zap.String("hint", "pprof is unauthenticated and exposes argv + heap; bind 127.0.0.1 and port-forward"))
		return
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Warn("daemon: pprof listener failed",
			zap.String("addr", addr), zap.Error(err))
		return
	}
	bound := ln.Addr().String()
	pprofAddr.Store(bound)
	applyContentionProfiling(logger, os.Getenv, runtime.SetBlockProfileRate, runtime.SetMutexProfileFraction)
	logger.Info("daemon: pprof endpoint open",
		zap.String("addr", bound),
		zap.String("hint", "go tool pprof -http=: http://"+bound+"/debug/pprof/heap"))
	http.HandleFunc("/debug/gortex/answer-path", serveAnswerPathDiagnostics)
	go func() {
		// Serve on the DefaultServeMux which net/http/pprof registers on.
		if err := http.Serve(ln, nil); err != nil && err != http.ErrServerClosed {
			logger.Warn("daemon: pprof serve exited", zap.Error(err))
		}
	}()
}

// applyContentionProfiling turns on the block and mutex profiles the pprof
// listener serves (/debug/pprof/block, /debug/pprof/mutex) when asked:
// GORTEX_DAEMON_BLOCK_PROFILE_RATE is runtime.SetBlockProfileRate's rate (a
// blocking event of that many nanoseconds or longer is sampled) and
// GORTEX_DAEMON_MUTEX_PROFILE_FRACTION is runtime.SetMutexProfileFraction's.
// Both are off by default: they cost on every contended operation, and they
// exist to attribute a stall, not to run all the time.
func applyContentionProfiling(logger *zap.Logger, getenv func(string) string, setBlock func(int), setMutex func(int) int) {
	if rate, err := strconv.Atoi(getenv("GORTEX_DAEMON_BLOCK_PROFILE_RATE")); err == nil && rate > 0 {
		setBlock(rate)
		logger.Info("daemon: block profile on", zap.Int("rate_ns", rate))
	}
	if fraction, err := strconv.Atoi(getenv("GORTEX_DAEMON_MUTEX_PROFILE_FRACTION")); err == nil && fraction > 0 {
		setMutex(fraction)
		logger.Info("daemon: mutex profile on", zap.Int("fraction", fraction))
	}
}

// serveAnswerPathDiagnostics reports the symbol-search answer path's counters
// on the (loopback, opt-in) pprof listener: how many generation rankings the
// in-memory full-text ranker served, and the generation-row preloads a route
// flip made. A measurement reads them per edit without a tool call.
//
// ?view_match=off|selective|on switches the ranker's one-MATCH read for the
// process, so one daemon can be measured both ways.
func serveAnswerPathDiagnostics(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	switch query.Get("view_match") {
	case "off":
		search.SetFTSViewMatchMode(search.FTSViewMatchOff)
	case "selective":
		search.SetFTSViewMatchMode(search.FTSViewMatchSelective)
	case "on":
		search.SetFTSViewMatchMode(search.FTSViewMatchOn)
	}
	// ?since=<unix ms> returns only the records newer than that instant, so a
	// poller that passes its last-seen instant receives each record once.
	since, _ := strconv.ParseInt(query.Get("since"), 10, 64)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answerPathSnapshot(since))
}

// answerPathSnapshot is the diagnostics payload: the counters, and the
// decision and preload records newer than since (unix ms; 0: all kept).
func answerPathSnapshot(since int64) map[string]any {
	installed, declined, preloads := graphview.RowsPreloadDiagnostics()
	wholeReads, wholeSparse := search.FTSWholeReadCounts()
	keptDocs, keptBytes := search.FTSKeptDocs()
	freshAdopted, freshRejected := search.FTSFreshCounts()
	nonASCIITokenized, wholeRefused := search.FTSNonASCIIRows()
	views := search.FTSViewMatchDecisions()
	wholes := search.FTSWholeReadDecisions()
	if since > 0 {
		keptViews := views[:0]
		for _, d := range views {
			if d.At > since {
				keptViews = append(keptViews, d)
			}
		}
		views = keptViews
		keptWholes := wholes[:0]
		for _, d := range wholes {
			if d.At > since {
				keptWholes = append(keptWholes, d)
			}
		}
		wholes = keptWholes
		keptPreloads := preloads[:0]
		for _, p := range preloads {
			if p.At.UnixMilli() > since {
				keptPreloads = append(keptPreloads, p)
			}
		}
		preloads = keptPreloads
	}
	return map[string]any{
		"fts_ranked_generations": search.FTSRankedGenerations(),
		"fts_view_match_reads":   search.FTSViewMatchReads(),
		"fts_view_match_mode":    int(search.CurrentFTSViewMatchMode()),
		"fts_whole_reads":        wholeReads,
		"fts_whole_reads_sparse": wholeSparse,
		"fts_kept_docs":          keptDocs,
		"fts_kept_bytes":         keptBytes,
		"fts_kept_evictions":     search.FTSDocsEvictions(),
		"fts_store_reads":        search.FTSStoreReads(),
		"fts_fresh_adopted":      freshAdopted,
		"fts_fresh_rejected":     freshRejected,
		"fts_non_ascii_rows":     nonASCIITokenized,
		"fts_whole_read_refused": wholeRefused,
		"fts_whole_read_recent":  wholes,
		"fts_view_match_recent":  views,
		"rows_preload_installed": installed,
		"rows_preload_declined":  declined,
		"rows_preload_recent":    preloads,
	}
}

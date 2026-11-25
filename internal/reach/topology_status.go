package reach

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// TopologyStatus is the state of the topology gates a mutation takes before it
// changes the graph: how far a repository mutation's hold reaches, and who
// holds or waits for the whole-graph gate and the gates of repositories in use.
type TopologyStatus struct {
	// Coupling is what is known of the store's repository coupling, which
	// decides how much a repository mutation holds. "ready": only the
	// repositories coupled with its own. "reading" or "unknown": the whole
	// graph, for each mutation that starts before the read lands. "missed": a
	// read failed, so every repository mutation holds the whole graph until
	// the process restarts. "unsupported": the store cannot report its
	// coupling, so every repository mutation holds the whole graph.
	Coupling string `json:"coupling"`
	// LastRead is the last read of the coupling that landed or missed.
	LastRead *CouplingRead `json:"last_read,omitempty"`
	// WholeGraph counts the holders of the whole-graph gate. Its writers
	// include repository mutations that started while the coupling was not
	// ready; they keep the whole graph until they finish.
	WholeGraph TopologyGate `json:"whole_graph"`
	// Repositories lists the repository gates in use, or the gates asked for.
	Repositories []RepositoryTopologyGate `json:"repositories,omitempty"`
	// Writers names every mutation that holds or waits for topology gates,
	// the longest held or waited first, so a wait can be traced to the
	// mutation it waits behind.
	Writers []TopologyWriter `json:"writers,omitempty"`
}

// TopologyWriter is one mutation that holds or waits for topology gates.
type TopologyWriter struct {
	// Repo is the repository the mutation changes, empty for a mutation of
	// the whole graph.
	Repo string `json:"repo,omitempty"`
	// WholeGraph says the mutation holds or waits for the whole graph: a
	// whole-graph mutation, or a repository mutation that began while the
	// coupling was not ready.
	WholeGraph bool `json:"whole_graph"`
	// State is "holding" or "waiting".
	State string `json:"state"`
	// SinceUnixMS is when the mutation took its gates or began to wait for
	// them, and ForMS how long it has held or waited so far.
	SinceUnixMS int64   `json:"since_unix_ms"`
	ForMS       float64 `json:"for_ms"`
}

// topologyWriter is a mutation's entry in topologyState.writers. It is
// guarded by the state's mu.
type topologyWriter struct {
	repo       string
	wholeGraph bool
	holding    bool
	since      time.Time
}

// noteWriter records a writer of prefix that starts waiting for scope now.
// t.mu is held.
func (t *topologyState) noteWriter(prefix string, scope []string) *topologyWriter {
	writer := &topologyWriter{repo: prefix, wholeGraph: scope == nil, since: time.Now()}
	if t.writers == nil {
		t.writers = make(map[*topologyWriter]struct{})
	}
	t.writers[writer] = struct{}{}
	return writer
}

// admit marks the writer as holding its gates from now on. The state's mu is
// held.
func (w *topologyWriter) admit() {
	w.holding, w.since = true, time.Now()
}

// TopologyGate counts one gate's readers, writers and waiting writers.
type TopologyGate struct {
	Readers        int `json:"readers"`
	Writers        int `json:"writers"`
	WritersWaiting int `json:"writers_waiting"`
}

// RepositoryTopologyGate is the gate of one repository.
type RepositoryTopologyGate struct {
	Repo string `json:"repo"`
	TopologyGate
}

// CouplingRead is one read of the repository coupling: of the whole store, or
// of the edges at one mutation's files ("frontier"); how it ended ("ready",
// "superseded" by a mutation that forgot the coupling meanwhile, "timed_out"
// past its budget, or "failed"), when, and how long it took.
type CouplingRead struct {
	Scope       string  `json:"scope"`
	Outcome     string  `json:"outcome"`
	EndedUnixMS int64   `json:"ended_unix_ms"`
	TookMS      float64 `json:"took_ms"`
}

const (
	couplingScopeStore    = "store"
	couplingScopeFrontier = "frontier"
)

// couplingRead records a coupling read; a zero ended means none yet.
type couplingRead struct {
	scope, outcome string
	ended          time.Time
	took           time.Duration
}

func endCouplingRead(scope string, started time.Time, err error, superseded bool) couplingRead {
	read := couplingRead{scope: scope, outcome: "ready", ended: time.Now()}
	read.took = read.ended.Sub(started)
	switch {
	case superseded:
		read.outcome = "superseded"
	case errors.Is(err, context.DeadlineExceeded):
		read.outcome = "timed_out"
	case err != nil:
		read.outcome = "failed"
	}
	return read
}

// Topology reports the topology gates of g and what is known of its
// repository coupling. With repos, it reports those repositories' gates,
// idle ones included; without, every repository gate in use.
func Topology(g graph.Store, repos ...string) TopologyStatus {
	t := topology
	t.mu.Lock()
	defer t.mu.Unlock()
	c := &t.coupling
	status := TopologyStatus{WholeGraph: TopologyGate{
		Readers: t.all.readers, Writers: t.all.writers, WritersWaiting: t.all.writersWaiting,
	}}
	_, supported := g.(repoCouplingSource)
	switch {
	case !supported:
		status.Coupling = "unsupported"
	case c.store != g:
		status.Coupling = "unknown"
	case c.ready:
		status.Coupling = "ready"
	case c.missed:
		status.Coupling = "missed"
	case c.seeding:
		status.Coupling = "reading"
	default:
		status.Coupling = "unknown"
	}
	if supported && c.store == g && !c.lastRead.ended.IsZero() {
		status.LastRead = &CouplingRead{
			Scope:       c.lastRead.scope,
			Outcome:     c.lastRead.outcome,
			EndedUnixMS: c.lastRead.ended.UnixMilli(),
			TookMS:      float64(c.lastRead.took.Microseconds()) / 1000,
		}
	}
	if len(repos) == 0 {
		for repo := range t.repos {
			repos = append(repos, repo)
		}
		sort.Strings(repos)
	}
	for _, repo := range repos {
		gate := RepositoryTopologyGate{Repo: repo}
		if held := t.repos[repo]; held != nil {
			gate.TopologyGate = TopologyGate{Readers: held.readers, Writers: held.writers, WritersWaiting: held.writersWaiting}
		}
		status.Repositories = append(status.Repositories, gate)
	}
	now := time.Now()
	for writer := range t.writers {
		state := "waiting"
		if writer.holding {
			state = "holding"
		}
		status.Writers = append(status.Writers, TopologyWriter{
			Repo:        writer.repo,
			WholeGraph:  writer.wholeGraph,
			State:       state,
			SinceUnixMS: writer.since.UnixMilli(),
			ForMS:       float64(max(now.Sub(writer.since), 0).Microseconds()) / 1000,
		})
	}
	sort.Slice(status.Writers, func(i, j int) bool {
		a, b := status.Writers[i], status.Writers[j]
		if a.ForMS != b.ForMS {
			return a.ForMS > b.ForMS
		}
		return a.Repo < b.Repo
	})
	return status
}

package analysis

import "time"

// Pace is a cooperative scheduling point for a long whole-graph analysis.
//
// The daemon runs at GOMAXPROCS=1, and a background analysis pass is one
// CPU-bound goroutine: it gets half of the only core whenever anything else
// is runnable, so an edit cycle or a tool call that overlaps it runs at half
// speed for as long as the pass lasts — minutes for a Leiden run on a large
// workspace. A pass that carries a Pace calls Tick once per row or node of its
// hot loops; every few ticks, and at most once per checkInterval, Tick asks
// shouldYield, and while it answers yes the pass parks (sleeps, so it is not
// runnable at all) and gives the core back.
//
// The bound on how long a waiting edit or tool call shares the core is
// checkInterval plus the time of checkEvery loop iterations; the loops Tick in
// do a bounded amount of work per iteration. A park lasts at most parkCap, after
// which the pass runs for one more checkInterval before it may park again, so a
// daemon that is never idle still finishes its pass.
//
// A nil *Pace is valid and never parks: every existing caller of the analysis
// functions keeps its behaviour. A Pace is used by one goroutine at a time.
type Pace struct {
	shouldYield func() bool

	checkEvery    int
	checkInterval time.Duration
	pollInterval  time.Duration
	parkCap       time.Duration

	ticks      int
	lastCheck  time.Time
	lastUnpark time.Time

	// onPark, when set, runs as a park begins. Tests only.
	onPark func()

	parks  int
	parked time.Duration
}

// Default pacing: a predicate check at most every 10 ms, a 5 ms poll while
// parked, and parks of at most 30 s.
const (
	defaultPaceCheckEvery    = 64
	defaultPaceCheckInterval = 10 * time.Millisecond
	defaultPacePollInterval  = 5 * time.Millisecond
	defaultPaceParkCap       = 30 * time.Second
)

// NewPace returns a Pace that parks while shouldYield reports true. A nil
// predicate returns a nil Pace.
func NewPace(shouldYield func() bool) *Pace {
	if shouldYield == nil {
		return nil
	}
	return &Pace{
		shouldYield:   shouldYield,
		checkEvery:    defaultPaceCheckEvery,
		checkInterval: defaultPaceCheckInterval,
		pollInterval:  defaultPacePollInterval,
		parkCap:       defaultPaceParkCap,
	}
}

// Tick is one iteration of a hot loop.
func (p *Pace) Tick() {
	if p == nil {
		return
	}
	p.ticks++
	if p.ticks < p.checkEvery {
		return
	}
	p.ticks = 0
	now := time.Now()
	if now.Sub(p.lastCheck) < p.checkInterval || now.Sub(p.lastUnpark) < p.checkInterval {
		return
	}
	p.lastCheck = now
	if !p.shouldYield() {
		return
	}
	p.park(now)
}

func (p *Pace) park(started time.Time) {
	p.parks++
	if p.onPark != nil {
		p.onPark()
	}
	for p.shouldYield() && time.Since(started) < p.parkCap {
		time.Sleep(p.pollInterval)
	}
	p.lastUnpark = time.Now()
	p.parked += p.lastUnpark.Sub(started)
}

// Stats reports how often and for how long the pass parked.
func (p *Pace) Stats() (parks int, parked time.Duration) {
	if p == nil {
		return 0, 0
	}
	return p.parks, p.parked
}

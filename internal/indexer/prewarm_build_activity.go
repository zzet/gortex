package indexer

import "sync"

// The process-wide activity guard protects maintenance. Prewarm needs the
// narrower repository scope: a busy family must not pause every other family.
var prewarmRepoBuilds = struct {
	sync.Mutex
	active map[string]int
}{active: make(map[string]int)}

func prewarmRepoBuildBegin(repoPrefix string) func() {
	prewarmRepoBuilds.Lock()
	prewarmRepoBuilds.active[repoPrefix]++
	prewarmRepoBuilds.Unlock()
	return func() {
		prewarmRepoBuilds.Lock()
		if prewarmRepoBuilds.active[repoPrefix]--; prewarmRepoBuilds.active[repoPrefix] == 0 {
			delete(prewarmRepoBuilds.active, repoPrefix)
		}
		prewarmRepoBuilds.Unlock()
	}
}

func prewarmRepoBuildActive(repoPrefix string) bool {
	prewarmRepoBuilds.Lock()
	defer prewarmRepoBuilds.Unlock()
	return prewarmRepoBuilds.active[repoPrefix] > 0
}

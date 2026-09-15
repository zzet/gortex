package resolver

// importReachableDirs includes direct imports and every transitive re-export.
// File identities control traversal; directory identities control the result.
// A directory can contain several barrels with different outgoing edges.
// Previously completed closures can be reused; this walk never publishes
// partial results, including when it encounters a cycle.
func importReachableDirs(root string, targets map[string]map[string]struct{}, completed map[string][]string) []string {
	if root == "" {
		return nil
	}
	// Completed slices are immutable. A sole target needs only the union
	// of the root directory and its target's complete directory list.
	if len(targets[root]) == 1 {
		for target := range targets[root] {
			if cached, ok := completed[target]; ok {
				dir := filePathDir(root)
				for _, d := range cached {
					if d == dir {
						return cached
					}
				}
				dirs := make([]string, len(cached)+1)
				dirs[0] = dir
				copy(dirs[1:], cached)
				return dirs
			}
		}
	}
	seen := make(map[string]struct{})
	dirs := make(map[string]struct{})
	var queue, reachable []string
	addDir := func(dir string) {
		if _, ok := dirs[dir]; !ok {
			dirs[dir] = struct{}{}
			reachable = append(reachable, dir)
		}
	}
	visit := func(file string) {
		if file == "" {
			return
		}
		if _, ok := seen[file]; ok {
			return
		}
		seen[file] = struct{}{}
		if cached, ok := completed[file]; ok {
			for _, dir := range cached {
				addDir(dir)
			}
			return
		}
		addDir(filePathDir(file))
		queue = append(queue, file)
	}
	visit(root)
	for i := 0; i < len(queue); i++ {
		for file := range targets[queue[i]] {
			visit(file)
		}
	}
	return reachable
}

// importRootOrder schedules imported descendants before their ancestors.
// Cyclic members have arbitrary order; only complete closures are cached.
func importRootOrder(roots []string, targets map[string]map[string]struct{}) []string {
	rootSet := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		rootSet[root] = struct{}{}
	}
	seen := make(map[string]struct{})
	var order []string
	type frame struct {
		file string
		exit bool
	}
	var stack []frame
	for _, root := range roots {
		stack = append(stack, frame{file: root})
		for len(stack) > 0 {
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if f.exit {
				if _, ok := rootSet[f.file]; ok {
					order = append(order, f.file)
				}
				continue
			}
			if _, ok := seen[f.file]; ok {
				continue
			}
			seen[f.file] = struct{}{}
			stack = append(stack, frame{file: f.file, exit: true})
			for target := range targets[f.file] {
				stack = append(stack, frame{file: target})
			}
		}
	}
	return order
}

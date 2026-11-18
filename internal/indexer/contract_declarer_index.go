package indexer

import (
	"slices"
	"strings"
)

func contractDeclaredHas(keys []string, key string) bool {
	_, found := slices.BinarySearch(keys, key)
	return found
}

// Only queries actually asked need an inverse declaration index. An absent
// entry scans the complete forward sets once, including a cached empty answer.
// Unit replacement updates every already-cached matching query immediately.
type contractDeclarerIndex struct{ queries map[string][]string }

func (d *contractDeclarerIndex) lookup(key string, units map[string]*contractUnit) []string {
	if strings.HasPrefix(key, "path:") {
		path := strings.TrimPrefix(key, "path:")
		if unit := units[path]; unit != nil && contractDeclaredHas(unit.declared, key) {
			return []string{path}
		}
		return nil
	}
	if paths, known := d.queries[key]; known {
		return paths
	}
	var paths []string
	for path, unit := range units {
		if unit != nil && contractDeclaredHas(unit.declared, key) {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	if d.queries == nil {
		d.queries = make(map[string][]string)
	}
	d.queries[key] = paths
	return paths
}
func (d *contractDeclarerIndex) add(unit *contractUnit) {
	for _, key := range unit.declared {
		paths, known := d.queries[key]
		if !known {
			continue
		}
		at, found := slices.BinarySearch(paths, unit.path)
		if found {
			continue
		}
		d.queries[key] = slices.Insert(paths, at, unit.path)
	}
}
func (d *contractDeclarerIndex) remove(unit *contractUnit) {
	if unit == nil {
		return
	}
	for _, key := range unit.declared {
		paths, known := d.queries[key]
		if !known {
			continue
		}
		if at, found := slices.BinarySearch(paths, unit.path); found {
			d.queries[key] = slices.Delete(paths, at, at+1)
		}
	}
}
func (d contractDeclarerIndex) clone(pool ...*contractProofStrings) contractDeclarerIndex {
	out := contractDeclarerIndex{queries: make(map[string][]string, len(d.queries))}
	for key, paths := range d.queries {
		out.queries[contractProofString(key, pool...)] = cloneContractDeltaStrings(paths, pool...)
	}
	return out
}

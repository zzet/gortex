// Package citest implements repository CI planning and test execution. It is
// imported only by the CI commands and tests, never by the application.
package citest

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

//go:embed policy.json
var policyJSON []byte

type Target struct {
	Runner string `json:"runner"`
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
}
type Policy struct {
	Targets    []Target          `json:"targets"`
	Partitions []string          `json:"partitions"`
	Groups     map[string]string `json:"groups"`
}
type MatrixRow struct {
	OS        string `json:"os"`
	Partition string `json:"partition"`
}
type Matrix struct {
	Include []MatrixRow `json:"include"`
}

func LoadPolicy() (Policy, error) {
	var p Policy
	if err := json.Unmarshal(policyJSON, &p); err != nil {
		return p, err
	}
	if len(p.Targets) != 3 || !slices.Equal(p.Partitions, []string{"indexer:1/2", "indexer:2/2", "mcp", "store", "rest"}) || len(p.Groups) != 3 {
		return p, fmt.Errorf("invalid CI policy")
	}
	seen := set{}
	for _, t := range p.Targets {
		if t.Runner == "" || t.GOOS == "" || t.GOARCH == "" || seen.has(t.Runner) {
			return p, fmt.Errorf("invalid CI target policy")
		}
		seen.add(t.Runner)
	}
	for _, g := range []string{"indexer", "mcp", "store"} {
		if p.Groups[g] == "" {
			return p, fmt.Errorf("missing CI group %s", g)
		}
	}
	if err := CheckLayout(p.FullMatrix().Include, p); err != nil {
		return p, err
	}
	return p, nil
}
func (p Policy) FullMatrix() Matrix {
	m := Matrix{Include: []MatrixRow{}}
	for _, t := range p.Targets {
		for _, part := range p.Partitions {
			m.Include = append(m.Include, MatrixRow{t.Runner, part})
		}
	}
	return m
}
func (p Policy) SelectedMatrix(module string, byTarget map[string][]string) Matrix {
	m := Matrix{Include: []MatrixRow{}}
	for _, t := range p.Targets {
		groups := set{}
		large := set{}
		for g, suffix := range p.Groups {
			name := module + suffix
			large.add(name)
			if slices.Contains(byTarget[t.Runner], name) {
				groups.add(g)
			}
		}
		for _, name := range byTarget[t.Runner] {
			if !large.has(name) {
				groups.add("rest")
			}
		}
		if t.GOOS == "darwin" && groups.has("indexer") {
			groups.add("rest")
		}
		for _, part := range p.Partitions {
			items, _ := ParsePartition(part, p)
			for _, item := range items {
				if groups.has(item.Group) {
					m.Include = append(m.Include, MatrixRow{t.Runner, part})
					break
				}
			}
		}
	}
	return m
}

type Partition struct {
	Group        string
	Shard, Count int
}

func ParsePartition(spec string, p Policy) ([]Partition, error) {
	var out []Partition
	seen := set{}
	for _, text := range strings.Split(spec, "+") {
		group, tail, sharded := strings.Cut(text, ":")
		item := Partition{Group: group, Count: 1}
		if group != "rest" && p.Groups[group] == "" || seen.has(group) {
			return nil, fmt.Errorf("bad partition group %q", group)
		}
		if sharded {
			a, b, ok := strings.Cut(tail, "/")
			i, e1 := strconv.Atoi(a)
			n, e2 := strconv.Atoi(b)
			if !ok || e1 != nil || e2 != nil || i < 1 || n < 1 || i > n || (group == "rest" && n != 1) || a != strconv.Itoa(i) || b != strconv.Itoa(n) {
				return nil, fmt.Errorf("bad partition shard %q", text)
			}
			item.Shard = i - 1
			item.Count = n
		}
		seen.add(group)
		out = append(out, item)
	}
	return out, nil
}
func CheckLayout(rows []MatrixRow, p Policy) error {
	byOS := map[string]map[string][]Partition{}
	for _, row := range rows {
		items, err := ParsePartition(row.Partition, p)
		if err != nil {
			return err
		}
		if byOS[row.OS] == nil {
			byOS[row.OS] = map[string][]Partition{}
		}
		for _, i := range items {
			byOS[row.OS][i.Group] = append(byOS[row.OS][i.Group], i)
		}
	}
	for os, groups := range byOS {
		if len(groups) != len(p.Groups)+1 {
			return fmt.Errorf("%s has incomplete CI groups", os)
		}
		for group, items := range groups {
			n := items[0].Count
			seen := map[int]bool{}
			for _, i := range items {
				if i.Count != n || seen[i.Shard] {
					return fmt.Errorf("%s %s has mixed or duplicate shards", os, group)
				}
				seen[i.Shard] = true
			}
			if len(seen) != n {
				return fmt.Errorf("%s %s has missing shards", os, group)
			}
		}
	}
	if len(byOS) == 0 {
		return fmt.Errorf("empty test layout")
	}
	return nil
}

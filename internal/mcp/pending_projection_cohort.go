package mcp

import (
	"errors"
	"maps"
)

// Every member was prepared and verified outside the registry mutex while its
// original source frontier was held. Expected is the exact old COW pointer;
// nil means that no entry existed. This is cache admission, never disk or
// journal authority. A refusal changes none of the registry.
type projectionCohortMember struct {
	Entry    *projectionEntry
	Expected *projectionEntry
}

func (p *pendingProjections) publishCohort(members []projectionCohortMember) error {
	if len(members) == 0 {
		return nil
	}
	if len(members) > projectionMaxFiles {
		return projectionOverFiles
	}
	if !p.enabled {
		return projectionDisabled
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	current := p.snap.Load()
	bytes := current.bytes
	files := len(current.entries)
	seen := make(map[string]struct{}, len(members))
	refuse := func(reason projectionRefusal) error { p.refused[reason]++; return reason }
	for _, member := range members {
		entry := member.Entry
		if entry == nil || entry.key == "" || entry.graphPath == "" || entry.size < 0 {
			return errors.New("projection cohort contains invalid entry")
		}
		if entry.checkedRetired.Load() {
			return refuse(projectionSuperseded)
		}
		if _, duplicate := seen[entry.key]; duplicate {
			return errors.New("projection cohort contains duplicate physical path")
		}
		seen[entry.key] = struct{}{}
		prior := current.entries[entry.key]
		if prior != member.Expected {
			return refuse(projectionSuperseded)
		}
		if prior != nil {
			if prior.graphPath != entry.graphPath {
				return refuse(projectionPathCase)
			}
			if prior.commitSeq > entry.commitSeq {
				return refuse(projectionSuperseded)
			}
			bytes -= prior.size
		} else {
			files++
		}
		if entry.size > projectionMaxBytes {
			return refuse(projectionOverBytes)
		}
		bytes += entry.size
	}
	if bytes > projectionMaxBytes {
		return refuse(projectionOverBytes)
	}
	if files > projectionMaxFiles {
		return refuse(projectionOverFiles)
	}
	next := &projectionSnapshot{entries: maps.Clone(current.entries), bytes: bytes}
	for _, member := range members {
		if member.Expected != nil && member.Expected.suspect.Load() {
			member.Entry.suspect.Store(true)
		}
		next.entries[member.Entry.key] = member.Entry
	}
	p.snap.Store(next)
	p.published.Add(int64(len(members)))
	return nil
}

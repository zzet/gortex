package mcp

import (
	"sort"
	"strings"
)

// An empty analyze result has more than one cause, and the caller cannot tell
// them apart from the number alone: the backing data may never have been
// built, it may cover only part of the scope, or the query may have been
// answered in full and found nothing. Only the last one is safe to act on, and
// it is the one an agent assumes.
//
// contract_tier.go answers that question for the contract tier. This file
// generalises the same three properties to any surface whose data is produced
// by a pass separate from indexing:
//
//  1. say the zero is not absence evidence;
//  2. name WHICH scope is in that state, not just that some are;
//  3. name the recovery, and rule out the recovery that looks obvious but
//     does nothing.
//
// The third property is the one that saves the most time. Every one of these
// surfaces is populated by an enrichment pass, and reindex_repository — the
// call a caller reaches for first — never runs one.
//
// Deliberately NOT built on graph.EnrichmentState. That marker is written only
// by the semantic (LSP) providers, and it refuses to record on a dirty tree or
// without a sha, so a repo enriched under either condition would be reported
// as never enriched. Each caveat here is instead derived from the same data
// the answer itself was computed from, which is stronger evidence and cannot
// contradict the number it accompanies.

// dataStateCaveatKey is the stable token a client matches on. It names the
// condition rather than the presentation, so callers can branch without
// parsing prose.
const dataStateCaveatKey = "data_state"

const (
	// dataStateNeverBuilt: the enrichment that populates this surface has
	// never run over the scope, so the empty result carries no information
	// about the question that was asked.
	dataStateNeverBuilt = "never_built"
	// dataStatePartial: it ran over part of the scope. Rows may exist and
	// still be an undercount, which is why this is reported separately
	// rather than folded into "built".
	dataStatePartial = "partial"
	// dataStateBuilt: the data is there and the answer is real. This is the
	// valuable half — it lets a caller say "actually nothing" with the same
	// confidence the tool has, instead of hedging every zero forever.
	dataStateBuilt = "built"
)

// dataStateCaveat is the structured caveat attached to an empty result whose
// backing data can legitimately be absent.
type dataStateCaveat struct {
	// State is one of the three constants above.
	State string
	// Source names the tier or enrichment the state describes, so a caller
	// that sees the same token from two different kinds can tell they share a
	// cause.
	Source string
	// Repos are the scopes in the reported state — the ones missing data for
	// never_built and partial. Empty for built.
	Repos []string
	// Recovery is the command that would produce the data. Empty when the
	// state is built, where there is nothing to recover.
	Recovery string
	// Note carries the reading a caller must not make, in prose.
	Note string
}

func (c dataStateCaveat) payload() map[string]any {
	out := map[string]any{
		"state":  c.State,
		"source": c.Source,
		"note":   c.Note,
	}
	if len(c.Repos) > 0 {
		out["repos"] = displayRepoPrefixes(c.Repos)
	}
	if c.Recovery != "" {
		out["recovery"] = c.Recovery
	}
	return out
}

// line renders the caveat for the compact text encoding, where a caller sees
// prose rather than a payload.
func (c dataStateCaveat) line() string {
	var b strings.Builder
	b.WriteString(dataStateCaveatKey + ": " + c.State + " (" + c.Source + ")")
	if len(c.Repos) > 0 {
		b.WriteString(" — " + strings.Join(displayRepoPrefixes(c.Repos), ", "))
	}
	b.WriteString(" — " + c.Note)
	if c.Recovery != "" {
		b.WriteString(" " + c.Recovery)
	}
	b.WriteString("\n")
	return b.String()
}

const blameDataStateSource = "blame_enrichment"

// blameRecovery names the pass that stamps authorship, and rules out the two
// calls a caller reaches for first. Indexing does not read git blame at all,
// so neither a re-index nor a re-track produces a single stamp.
const blameRecovery = "run `gortex enrich blame` (or `gortex enrich all`). reindex_repository and untrack/track will NOT stamp authorship: indexing never reads git blame, so a re-parse of every file still leaves it empty."

// ownershipDataState classifies an empty ownership answer from the counts its
// own scan produced.
//
// candidates and stamped are per-repo tallies over exactly the nodes the
// handler considered: candidates counts every symbol that passed the kind and
// path filters, stamped counts those carrying blame authorship. owners is the
// number of distinct owners found before the min_symbols filter — non-zero
// there with no rows means the data was fine and the threshold removed the
// answer, which is a third silent zero this handler used to render
// identically to the other two.
func ownershipDataState(candidates, stamped map[string]int, owners int) dataStateCaveat {
	var unstamped []string
	covered := 0
	total := 0
	for repo, n := range candidates {
		if n == 0 {
			continue
		}
		total++
		if stamped[repo] > 0 {
			covered++
			continue
		}
		unstamped = append(unstamped, repo)
	}
	sort.Strings(unstamped)

	switch {
	case total == 0:
		// Nothing was in scope to be owned. Blame state is irrelevant: no
		// enrichment would put a row here, so reporting one as missing would
		// send the caller after the wrong thing.
		return dataStateCaveat{
			State:  dataStateBuilt,
			Source: blameDataStateSource,
			Note:   "no symbol matched the kind / path_prefix filters, so this empty result is about the filters rather than about authorship data.",
		}
	case covered == 0:
		return dataStateCaveat{
			State:    dataStateNeverBuilt,
			Source:   blameDataStateSource,
			Repos:    unstamped,
			Recovery: blameRecovery,
			Note:     "not one symbol in scope carries a blame stamp, so this empty result is NOT evidence that nobody owns this path — authorship is stamped by a separate enrichment pass and is absent until it runs.",
		}
	case len(unstamped) > 0:
		return dataStateCaveat{
			State:    dataStatePartial,
			Source:   blameDataStateSource,
			Repos:    unstamped,
			Recovery: blameRecovery,
			Note:     "these repositories hold candidate symbols but no blame stamps, while others in scope are stamped. Any ownership answer over this scope is an undercount, and an empty one says nothing about the unstamped repositories.",
		}
	case owners > 0:
		return dataStateCaveat{
			State:  dataStateBuilt,
			Source: blameDataStateSource,
			Note:   "authorship is stamped across the scope and owners were found; every one of them fell below min_symbols. Lower min_symbols to see them — this empty result is a threshold, not an absence.",
		}
	default:
		return dataStateCaveat{
			State:  dataStateBuilt,
			Source: blameDataStateSource,
			Note:   "authorship is stamped across the scope, so this empty result is a real answer: no symbol in it carries an owner email and timestamp.",
		}
	}
}

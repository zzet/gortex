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

// The three states name what was OBSERVED in the answer's own scope, never
// what a pass did or did not do. That distinction is not pedantry: these
// enrichments are best-effort per file and report success while skipping every
// file they could not read, so "no data present" and "never ran" are not the
// same claim and only the first one is observable from here. Naming a state
// after run history would publish a history this code cannot see, and hand the
// caller a recovery that may not clear it.
const (
	// dataStateAbsent: nothing in scope carries this data. Whether the pass
	// never ran or ran and produced nothing is outside what can be seen here.
	dataStateAbsent = "absent"
	// dataStatePartial: some of the scope carries it and some does not. Rows
	// may exist and still be an undercount, which is why this is reported
	// separately rather than folded into complete.
	dataStatePartial = "partial"
	// dataStateComplete: every symbol the pass admits carries data, so the
	// answer is whole. This is the valuable half — it lets a caller say
	// "actually nothing" with the same confidence the tool has, instead of
	// hedging every zero forever.
	dataStateComplete = "complete"
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
	// state is complete, where there is nothing to recover.
	Recovery string
	// Eligible and Stamped size the shortfall: how many symbols the pass
	// admits in scope, and how many carry data. A caller deciding whether to
	// act on a partial answer needs the magnitude, not only the verdict.
	Eligible int
	Stamped  int
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
	if c.Eligible > 0 {
		out["symbols_eligible"] = c.Eligible
		out["symbols_stamped"] = c.Stamped
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
const blameRecovery = "run `gortex enrich blame` (or `gortex enrich all`). reindex_repository and untrack/track will NOT stamp authorship: indexing never reads git blame, so a re-parse of every file still leaves it empty. If this state survives the pass, the remaining files are ones git could not blame — an unborn commit, an untracked or generated file — rather than ones nobody has enriched yet."

// ownershipDataState classifies an ownership answer from the counts its own
// scan produced.
//
// candidates and stamped are per-repo tallies over exactly the nodes the
// handler considered: candidates counts every symbol that passed the kind and
// path filters AND that the blame pass would admit (blame.Eligible), stamped
// counts those carrying authorship. owners is the number of distinct owners
// found before the min_symbols filter — non-zero there with no rows means the
// data was fine and the threshold removed the answer, which is a third silent
// zero this handler used to render identically to the other two.
//
// Coverage is compared, not presence. One stamped symbol proves the pass ran
// over that repository; it does not prove the repository is covered.
// blame.EnrichGraph is best-effort per file — a file git cannot blame is
// skipped and the pass reports success — so a single repository routinely
// holds both stamped and unstamped eligible symbols. Reading a single stamp as
// full coverage published exactly the misleading non-empty undercount this
// caveat exists to prevent.
//
// The counting population is blame's own admission set for the same reason in
// reverse: a symbol the pass never looks at is not a coverage hole, and
// counting it would report a shortfall no enrichment could close.
// thresholdClause is appended whenever min_symbols emptied an answer that had
// owners in it. It is a SECOND, unrelated reason the response is empty, and
// reporting only the coverage shortfall would send a caller to re-run an
// enrichment that would not put the rows back.
const thresholdClause = " Separately: owners were found and min_symbols removed every one of them, so this particular response is empty for a second reason that no enrichment will change — lower min_symbols to see them."

func ownershipDataState(candidates, stamped map[string]int, owners, rows int) dataStateCaveat {
	var unmined, incomplete []string
	covered := 0
	total := 0
	eligibleTotal, stampedTotal := 0, 0
	for repo, n := range candidates {
		if n == 0 {
			continue
		}
		total++
		eligibleTotal += n
		got := stamped[repo]
		stampedTotal += got
		switch {
		case got == 0:
			unmined = append(unmined, repo)
		case got < n:
			incomplete = append(incomplete, repo)
		default:
			covered++
		}
	}
	unstamped := append(append([]string{}, unmined...), incomplete...)
	sort.Strings(unstamped)
	sort.Strings(unmined)

	// Both causes are live at once when coverage is short AND the threshold
	// emptied the rows, so the note is composed rather than chosen.
	thresholdEmptied := rows == 0 && owners > 0

	switch {
	case total == 0:
		// Nothing was in scope to be owned. Blame state is irrelevant: no
		// enrichment would put a row here, so reporting one as missing would
		// send the caller after the wrong thing.
		return dataStateCaveat{
			State:  dataStateComplete,
			Source: blameDataStateSource,
			Note:   "no symbol matched the kind / path_prefix filters, so this empty result is about the filters rather than about authorship data.",
		}
	case stampedTotal == 0:
		note := "not one symbol in scope carries a blame stamp, so this result is NOT evidence that nobody owns this path — authorship comes from a separate pass and none of it is present here."
		if thresholdEmptied {
			note += thresholdClause
		}
		return dataStateCaveat{
			State:    dataStateAbsent,
			Source:   blameDataStateSource,
			Repos:    unmined,
			Recovery: blameRecovery,
			Eligible: eligibleTotal,
			Stamped:  stampedTotal,
			Note:     note,
		}
	case len(unstamped) > 0:
		note := "these repositories hold symbols the blame pass admits but that carry no authorship — the pass is best-effort per file and skips what it cannot blame, silently. Any ownership answer over this scope is an undercount: whatever rows it returns are real, and the symbols behind the shortfall are invisible to them."
		if thresholdEmptied {
			note += thresholdClause
		}
		return dataStateCaveat{
			State:    dataStatePartial,
			Source:   blameDataStateSource,
			Repos:    unstamped,
			Recovery: blameRecovery,
			Eligible: eligibleTotal,
			Stamped:  stampedTotal,
			Note:     note,
		}
	case thresholdEmptied:
		return dataStateCaveat{
			State:  dataStateComplete,
			Source: blameDataStateSource,
			Note:   "authorship is stamped across every symbol the pass admits, so nothing is missing." + thresholdClause,
		}
	default:
		return dataStateCaveat{
			State:  dataStateComplete,
			Source: blameDataStateSource,
			Note:   "authorship is stamped across every symbol the pass admits, so this result is whole: what it shows is what there is.",
		}
	}
}

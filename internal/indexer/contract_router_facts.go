package indexer

import (
	"path/filepath"

	"github.com/zzet/gortex/internal/contracts"
)

// The proof records the actual join result, including a join that found no
// mount. Unconsumed facts and source line counts cannot invalidate it.
type contractRouterProof struct {
	repo    string
	records []contracts.Contract
	digest  string
}

func contractRouterCensus(file ContractFollowupFile) bool {
	switch file.Language {
	case "python", "typescript", "javascript":
		return true
	}
	switch filepath.Ext(file.Path) {
	case ".py", ".ts", ".tsx", ".js", ".jsx":
		return true
	}
	return false
}

func contractRouterSupported(file ContractFollowupFile, records []contracts.Contract) bool {
	if contractRouterCensus(file) {
		return true
	}
	// Outside the unconditional scanner census, the legacy pass only reads
	// source belonging to an HTTP route. An unrelated Go/Java/etc. unit must
	// retain its normal content-only proof across comment and body saves.
	for _, record := range records {
		if record.Type == contracts.ContractHTTP {
			switch record.Meta["framework"] {
			case "fastapi/flask", "flask", "express", "nestjs", "gin/echo/chi", "spring", "rails", "laravel", "axum", "fiber", "vapor", "actix":
				return true
			}
		}
	}
	return false
}

func contractRouterEligible(reg *contracts.Registry) bool {
	for _, record := range reg.All() {
		if record.Type != contracts.ContractHTTP {
			continue
		}
		switch record.Meta["framework"] {
		case "fastapi/flask", "express", "nestjs":
			return true
		}
	}
	return false
}

// Match the scanner's file union: eligible census languages plus every route
// owner. Facts were extracted and proven with their units, including files
// whose raw contract set is empty. This pass never reads source or core rows.
func (c *contractConvergence) routerFacts(repo string, reg *contracts.Registry) map[string]contracts.RouterFacts {
	owners := make(map[string]bool)
	for _, record := range reg.All() {
		if record.Type == contracts.ContractHTTP {
			owners[record.FilePath] = true
		}
	}
	out := make(map[string]contracts.RouterFacts)
	for path, unit := range c.units {
		file := c.files[path]
		if unit == nil || unit.excluded || !c.live[path] || file.RepoPrefix != repo || unit.router == nil {
			continue
		}
		if contractRouterCensus(file) || owners[path] {
			out[path] = *unit.router
		}
	}
	return out
}

func (c *contractConvergence) routerProofsHold(proofs []contractRouterProof) bool {
	for _, proof := range proofs {
		reg := contracts.NewRegistry()
		reg.AddAll(cloneContractRecords(proof.records), proof.repo)
		contracts.JoinRouterPrefixFacts(reg, c.routerFacts(proof.repo, reg))
		digest, ok := contractAnswerDigest(reg.All())
		if !ok || digest != proof.digest {
			return false
		}
	}
	return true
}

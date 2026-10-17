package contracts

import (
	"encoding/json"
	"sort"
)

const MatchDependencyKeyVersion = "contract-match-dependencies-v1"

// MatchDependencyKeys identifies conservative matcher cohorts, including
// missing-counterpart membership. It uses the matcher's own normalization:
// raw IDs alone do not cover canonical RPC/tRPC joins or ambiguity changes.
// These are dependency buckets, not a claim that a bucket is already matched.
func MatchDependencyKeys(record Contract) []string {
	key := func(kind, value string) string {
		encoded, _ := json.Marshal([]string{MatchDependencyKeyVersion, record.EffectiveWorkspace(), record.EffectiveProject(), kind, value})
		return string(encoded)
	}
	keys := []string{key("exact", record.ID)}
	if isRPCFamily(record) {
		service, _ := rpcServiceMethod(record)
		if service != "" {
			keys = append(keys, key("rpc_service", service))
		}
	}
	if record.Type == ContractTRPC {
		_, procedure := trpcRouterProcedure(record)
		if procedure != "" {
			keys = append(keys, key("trpc_procedure", procedure))
		}
	}
	sort.Strings(keys)
	return keys
}

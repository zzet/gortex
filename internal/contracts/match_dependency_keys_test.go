package contracts

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMatchDependencyKeysCanonicalAndNegativeMembership(t *testing.T) {
	containsSame := func(a, b Contract) bool {
		for _, x := range MatchDependencyKeys(a) {
			for _, y := range MatchDependencyKeys(b) {
				if x == y {
					return true
				}
			}
		}
		return false
	}
	base := Contract{RepoPrefix: "a", WorkspaceID: "workspace", ProjectID: "project", Role: RoleConsumer, Type: ContractGRPC, ID: "grpc::billing.v1.Users::GetUser"}
	provider := base
	provider.Role = RoleProvider
	provider.RepoPrefix = "c"
	provider.Type = ContractThrift
	provider.ID = "thrift::Users::getUser"
	require.True(t, containsSame(base, provider), "a missing provider added with another raw ID must invalidate the consumer's canonical cohort")
	provider.ID = "thrift::Users"
	require.True(t, containsSame(base, provider), "service-level provider fallback participates")
	provider.ProjectID = "other"
	require.False(t, containsSame(base, provider), "soft boundary is part of membership")
	trpc := base
	trpc.Type = ContractTRPC
	trpc.ID = "trpc::api.getUser"
	other := trpc
	other.ID = "trpc::userRouter.GetUser"
	other.Role = RoleProvider
	require.True(t, containsSame(trpc, other), "procedure cohort includes namespace ambiguity/new candidates")
	require.False(t, containsSame(base, trpc), "RPC and tRPC must remain separate")
}

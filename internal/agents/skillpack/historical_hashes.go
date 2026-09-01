package skillpack

// PreWorktreeAgentSkillHashes are the last Agent Skills bodies shipped
// before worktree/branch routing guidance was added. Codex and OpenCode
// share this rendering.
var preWorktreeAgentSkillHashes = singletonHashes(map[string]string{
	"gortex-add-test": "f9c35446b260c9a27029efc78ca8ed2a2a3d25817fd84357a985c68226dea241", "gortex-architecture-review": "8b4988b8d403ffbce5e4f5f6fe38040c2beec2ec5ebf9de5847ce3cd8d8c6ace",
	"gortex-cli": "3f3fe05650efc6245ef21dd0d346c7f6ab603fab6160f4b9a1ab3aae9c1a6b60", "gortex-co-change": "79d6b90d90598f1bdf964e7f382cfe9ddd7228246d27665bbc2e91742a6064ab",
	"gortex-cross-repo-usage": "eb7a9c96ab61eea8aca0b4f9c742fb005c584b8af10fd69d196e7da1610f3f47", "gortex-dataflow-trace": "df3cdb6f5a1b889467f0f96f19985d6165c7b4c52fca342088f9cc0f82dd9b5b",
	"gortex-debug": "d645582b99a5e0467426cf6cba5bb03553eb0b6222017510dd157997a32e86ad", "gortex-episode-replay": "af58d86e5a4aca9dfe78e1f8cf4bb77250e41cdebbef26e52702b7384ccf8673",
	"gortex-explore": "175782c52ca5b6963d6037005862f86a78d267cb8178003c3b0fc12cf8374e83", "gortex-extract-function": "6a5d754fe5defc0115c152832307b105526c68ba226a66253d7f1a3e5977aca0",
	"gortex-fix-all": "d545949f68487191217b4892545c75ac9de0f32372a17c1aa71531b10861bb66", "gortex-guide": "72b6a5d2d14f935319643d5ab2a25be02eb6b883fbd66311d550e19a7f86a134",
	"gortex-impact": "b04b04ba8128ed05a86e53820bd9749763af559950590fb85e22a973f4cee72a", "gortex-incident-investigation": "70a31ce6ea099dfb5e248f9aa08f1f937fe3b0898f172a3fceb7a94b194d7f0c",
	"gortex-onboarding": "79b7433b1d28109201ae02613b72e47dd343a4c45db765562a2a77fad1c3dd55", "gortex-pr-review": "ba4c908c0767c94d89d92330c508238ba69dc5d9b6d606436edad76a8e3fef20",
	"gortex-pr-review-agent": "5b62d75b27d2031a23eb05e9748dc6e785e3067494dbdc8f4a7a246c2a856cb9", "gortex-quality-audit": "4ac5beff28652cb1f3c215a00cf3c72407af2cc39972df67118ffe8b600f2c23",
	"gortex-refactor": "68b71e365c575b79620ab941113a82e1ea562c0fc9dfd5ee6b89edaeee769ae3", "gortex-rename": "56573eeab6f9705179c06a793dceafc15167dd11baa0880f1196a6f26cbc4c54",
	"gortex-safe-edit": "da4b98c09db476229dea1ae3e10676ce2b9c28a637e389ef277ab47f80148a63",
})

// PreWorktreeClaudeSkillHashes are the corresponding Claude/Copilot bodies.
var preWorktreeClaudeSkillHashes = singletonHashes(map[string]string{
	"gortex-add-test": "8951a4e35690e88433da5b8cf6bfdb9791282917f03da8667e2f5eddfa09dc77", "gortex-architecture-review": "d76efac123c3f20a947bec17618a98356b33fb5a2dab26ad8c1c72ade3b21abb",
	"gortex-cli": "d4fd21fa08e88d5feef35bc68c7e777beaf25fe07f0f5ec8c686d6d5a0865259", "gortex-co-change": "c7f56a2ffdb4c135a03938b806c07bd7414063faf7b41d46b5c300e03bc78747",
	"gortex-cross-repo-usage": "ea2a619fbc3b7bbc3e1bdd9c2abad4db49eead127fa415321a831f3d455170c7", "gortex-dataflow-trace": "070f7e3013eea4e53054807ce0870b7cca0a7594d29cdf941e188f82edf4d532",
	"gortex-debug": "a9614f91129b0437321fbf0cfcba2bdc8c13b1d6fe57429909cf7297e02f96f0", "gortex-episode-replay": "fa66ded5c67d013b711289c4be23fc1cbdae39686adc7e0c0f3e18ace87b6359",
	"gortex-explore": "c07e0e30f3878d140b023db763d5dafbe42c054e8b49fa02741250fc4e7f7c4e", "gortex-extract-function": "5a35a75a65b49d73c8f02cbb05d236462cdeea06e06fffa0f86d53742eeb0a03",
	"gortex-fix-all": "4b68e7d00f3f50b13227e1cf50200fbc10022068afbd83b46ee928504b1c0ecd", "gortex-guide": "8b203d02982285c7ed2a6324b76a65d0eb24e579fbbb26ecd06b58ae69e348db",
	"gortex-impact": "c729e7dfbd1d0f7c74e3090106021677086a9ae5d3060672eed317f6ae9d93ab", "gortex-incident-investigation": "d66b138dc93c11e26035a5fa7c3aa04e42a1dccb1d291dbd198807755c6bc016",
	"gortex-onboarding": "f9fc6000b4f89e2b9a9afc80c78092a618b203dd6b78eb2482c7e52d1fa36baa", "gortex-pr-review": "5c6ed36bc146fb4a771e73906b8d3c04648a8b70119abf4d4adf5c54e07f6eb3",
	"gortex-pr-review-agent": "4aeaad252903dc1b9ab67147947029ce450a8ebb201a3ea93da0b4a55f6d58ed", "gortex-quality-audit": "9301ef2cd66b384f2d06d51966b788a5c770fe1331efb71a2cea60c062354613",
	"gortex-refactor": "a0df6dc0c78ecb78f9d68653cc305b74eda1bca967a333a24a86ac0266b71811", "gortex-rename": "23d4e63223b9c8451f16ddb20cd1dc74976523a60afcac970f378aee9b7c4d3e",
	"gortex-safe-edit": "3eba357507bc25f87a680a2bd7be5620dafd90581a5475f00af0b8274a0f23c0",
})

// PreWorktreeHermesSkillHashes are the last Hermes renderings before the policy.
var preWorktreeHermesSkillHashes = singletonHashes(map[string]string{
	"gortex-add-test": "4676d1ae00484b599bd3ff63cda0c6572409dee00b743ab4939362579189c89c", "gortex-architecture-review": "a12184571cd9c83673e0952d9592b69a0266515b8b4c36d779d500a4b06fc0ae",
	"gortex-cli": "9d26b1882dfed73ad6c4fe25af6fa16d1863c114dd13261a7f6ffbfc2a58b86d", "gortex-co-change": "7115478f66c937a55757b93035faaa1bffc99302995aeb7b1aa74d77360e0385",
	"gortex-cross-repo-usage": "6e47c5d55edd9ccd8c6034305a32a9f11ec9ba27a986315e6ca2ec29ae98b690", "gortex-dataflow-trace": "55ecce4c14eed309863e0c9f6cd6b0c28c18bd0ff44c8e99f81af1f359078b00",
	"gortex-debug": "51e586a3a0dd5fad6060e99a7e8547a9be310732c0c1ccfe9960a80321849a23", "gortex-episode-replay": "b0cffaa932878186b6961f66a6e2910e2b5d98605514807391a0e24046677351",
	"gortex-explore": "ecea8962e1aba48af3a4026e6edb44ea67baca970526365bc84770996d52b19b", "gortex-extract-function": "bf74dc4830dddef3199351a64f549cc05384daa40d67d76516befae7c4eacf09",
	"gortex-fix-all": "b7ab7eee16f0705c18d8241f7ca024b341c00a26ad9a915283aa7dd51146b77d", "gortex-impact": "1fb72827f8e5df68c4247af5d2cd925bb972a8cdde8a0345017fc7f88791f270",
	"gortex-incident-investigation": "b9d8d3bdc66082fda514cb0a0dde0b4e80def6041f7cb8f60d85bd2d8df5e093", "gortex-onboarding": "bd2856edc8daa2d0d3a2337fddb48cb245b01741ec96e91c41f4d3a67789ea3e",
	"gortex-pr-review": "28807faf8ce87ac6f33c72bad211c7dd1b872a216be59f3f60917ab2af88d85d", "gortex-pr-review-agent": "78ff383e288cbfab4ed83d77c13aa02ab0d3714c1194ec5fd5853c348191e06c",
	"gortex-quality-audit": "6261a42265f6c9796316e9f34ee189f093f92713d0048a5a2684e648dc132859", "gortex-refactor": "471c0c3bd77fd0d66255a324a89b2005172fc195b182aff656510eb0c6d1c99d",
	"gortex-rename": "452f79c7dbe32b4c6e721acddcd17c80bcb0b534807546dfa29d69848e0345ea", "gortex-safe-edit": "22ed1f00a4b679f86b0baa5aee9c3466845fcc5c477da3a6716c333ee592c156",
})

func singletonHashes(hashes map[string]string) map[string][]string {
	out := make(map[string][]string, len(hashes))
	for id, hash := range hashes {
		out[id] = []string{hash}
	}
	return out
}

func cloneHashes(source map[string][]string) map[string][]string {
	out := make(map[string][]string, len(source))
	for id, hashes := range source {
		out[id] = append([]string(nil), hashes...)
	}
	return out
}

// PreWorktreeAgentSkillHashes returns a defensive copy of the Codex/OpenCode hashes.
func PreWorktreeAgentSkillHashes() map[string][]string {
	return cloneHashes(preWorktreeAgentSkillHashes)
}

// PreWorktreeClaudeSkillHashes returns a defensive copy of the Claude/Copilot hashes.
func PreWorktreeClaudeSkillHashes() map[string][]string {
	return cloneHashes(preWorktreeClaudeSkillHashes)
}

// PreWorktreeHermesSkillHashes returns a defensive copy of the Hermes hashes.
func PreWorktreeHermesSkillHashes() map[string][]string {
	return cloneHashes(preWorktreeHermesSkillHashes)
}

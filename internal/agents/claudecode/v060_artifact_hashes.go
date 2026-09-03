package claudecode

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/zzet/gortex/internal/agents"
)

// These hashes are byte-for-byte fingerprints of the artifacts shipped by
// gortex v0.60.0 before the compact public surface. They let upgrades replace
// only untouched Gortex files while preserving every customized copy. The
// concrete retirement gate is documented in docs/versioning.md.
var v060GlobalSkillHashes = map[string]string{
	"gortex-add-test":               "ac7088f6146395d15e70b011e4150a1708614e03763618202cb65367f807018c",
	"gortex-architecture-review":    "01594df88d730aab382dd3b2ef5d99c4cb03ce62ed1e04e56ef02049325efdcd",
	"gortex-cli":                    "8c71c2290a1695fc2c2461575ff949f0d33a659fff2d291e728c8109fb81d16b",
	"gortex-co-change":              "3bdf72fac5521b25785d059b80d3b8e37ed42c0128469cb69d0ce92723ea236c",
	"gortex-cross-repo-usage":       "4039ab9879a065b6c203796a0a4f0a3c28994e50f1e15524cf678e5123d9641e",
	"gortex-dataflow-trace":         "4eb7f28c754758b5926d7cd2b4cad630fa60aa98d3fbb81f49369174b9abb100",
	"gortex-debug":                  "3af79a0b0be77d0660ca8f8cb0343500cff6743b6e9fcfddbc4d542afdffd167",
	"gortex-episode-replay":         "d120d0948085837bf7a1066d16e0250999eff8f0f1acc067b970e51c9fddd598",
	"gortex-explore":                "0edc3c1fbd82483b993f960f4947c07bbcb6c0f5d3d230fd459425e69e874b97",
	"gortex-extract-function":       "637c84c094f221b3841685c5165c2eebc34ef7ebd06a5af07c54c7c600467ccd",
	"gortex-fix-all":                "33fc9b35567033434354188704aba70f952b7950e3df6b512d4df0adf87f9089",
	"gortex-guide":                  "d7cba2d96a2bba7c96df17e2f410e8de140764ec24c466123621b34cbb2ed7c2",
	"gortex-impact":                 "2111222dc7fcd4227db773ccb5ac8dca45f867f3ad2fba88b260569d5237a1ed",
	"gortex-incident-investigation": "30572b4534c5aa17a17789b52e1a74ab142e0b81e78b3fb85af207d4835d69f3",
	"gortex-onboarding":             "8803580cfefdbf45bb3a87e760340134f41dc5378cb5881a33bc7bfc054a448a",
	"gortex-pr-review":              "f87a000a7776e4db83c20f62372659efb354a4bca5d800cdd520ba05c1316cbe",
	"gortex-pr-review-agent":        "2be09fca75a79f7574cb57c5990aa87e27d72845cb8888a47b25e3129123ebb0",
	"gortex-quality-audit":          "51424a58bf9bbe9769997524feff7d5db07878d7012e49e8a5fcb855484b311a",
	"gortex-refactor":               "5d72e85a53df7cade5776cf0499ea5a2cb5a61f93bb89121abb5e829e1648eb6",
	"gortex-rename":                 "6416d2ee5a1c8c6c4231c4e7bc2de92cd6723bd968b4d72969b446df3920f807",
	"gortex-safe-edit":              "ccfc21a7ffc81ef8407b97cd54509f18226a09ee46a0d8fa3acc4265422f935e",
}

var v060SlashCommandHashes = map[string]string{
	"gortex-add-test.md":               "5dc170d682bfd0e9023b1d4f906378aa9bd017d11d1891e12166bc39f231a6d8",
	"gortex-architecture-review.md":    "79e168a3bf9a7cc015a565b33b9e78befc96fc3ddf57f2de716caa5490154f6c",
	"gortex-co-change.md":              "95a0176b2d231295d0936aa7e2334ee55ffa6aeddbd728f920732145cda3b784",
	"gortex-cross-repo-usage.md":       "00bdcb9545f95d2bcc30f67a815760577610fca6947b4d90aa80c75386f6bdcd",
	"gortex-dataflow-trace.md":         "a6d4d3a2d19ac9d628b1b3821f40e85bd17608690d2e8275011c5c3d857b8547",
	"gortex-debug.md":                  "4236422d3c14d3f0faf57648e00aa32e7c98e0c00f68dd29dfaa7c5c3a84a70f",
	"gortex-episode-replay.md":         "3ff36a94763529100b7acb0ee24d1134eb33ebe6884d98ff4fbd424510d69e40",
	"gortex-explore.md":                "9154a6bbcfc60c198890f29355faea3d8cf398c6d574b9d8ada905121663f39c",
	"gortex-extract-function.md":       "aa0d3c453b5e2ed5ec8bc28ef83e2ed6983126b53c79bcecdba205d7ff8c4e08",
	"gortex-fix-all.md":                "a3dd6bfa6a8a14915e766685e051e319b4193ba8e52d74811af0cb7fa5ebb58b",
	"gortex-guide.md":                  "068a0f2e7911bf0404888235490c97baf3a6632e9ef8eced171b09543851f480",
	"gortex-impact.md":                 "4c02060ae0868363d27c9b4b42a6a54ac432ac39e1687d4c2b77d5e4d740ee9d",
	"gortex-incident-investigation.md": "7a5b5d2be17eb76477acefc9beb41752b07cfb6a15aaa50e427a500a4b956638",
	"gortex-onboarding.md":             "4f09948418b3754d03321a81201185f744b55bb850cd2d18a5ace11708c97fe6",
	"gortex-pr-review-agent.md":        "776a9af046c26556561f8b9e9f92ac5c4737507c6380d5c274507294e9f72d26",
	"gortex-pr-review.md":              "0954c788915b270646c9593b1a4d890411a408bf1a6eaf76b3e40d8eb05dcfe0",
	"gortex-quality-audit.md":          "bd6bdb90fa9466c836c95755724bd359a7aae51d6818d7941da3063c2eee197b",
	"gortex-refactor.md":               "313fdc2f7daf28891ce19a94164c71a6e00eae225d76143251f3794b4d1d7a34",
	"gortex-rename.md":                 "5700aa7b1bdaba861c8e7f5b59623f17a49935de6bd8203bf7142a58499cbcd3",
	"gortex-safe-edit.md":              "5e487527a55816d64c23cc0fa080e024af3a771ed68e96c28a16940693f0b91d",
}

var v060SubAgentHashes = map[string]string{
	"gortex-impact.md": "bcf2bebeee1a932d1896e14a8de8ae8892191aed8683e233ca1d8ffa5276dae5",
	"gortex-search.md": "40cb39e36419993b2d75a9a13f363542bf392168e2d299e201165da7459f715b",
}

var preWorktreeSlashCommandHashes = map[string]string{
	"gortex-add-test.md": "5f146a2ca1ed064eb4988568b93cbf77bb0fce92967151d95d3db00d6f13dd6f", "gortex-architecture-review.md": "12c074cf6cdd2d18962980d5417ec2749379a4d2117acdaecc74860ec4f0acc1",
	"gortex-co-change.md": "4509d967766bc956be79638b01f0d9779994953bc986a320ad52e4b6edc5a362", "gortex-cross-repo-usage.md": "f4e62b30555965628db275d40d85f09cfc895d3735b238b4c9ab7347bde559a9",
	"gortex-dataflow-trace.md": "46848c39e937cc218a673a25243b7110562916e14f65e01ebcf4f175e96e9013", "gortex-debug.md": "3ae4d4db496d5a56d23083d878e342372050702da435d44998b24fa3d5885d32",
	"gortex-episode-replay.md": "d746b112628330ec037049aaee17cb5e715be1c93ee8cc0c9ec985357fb5c091", "gortex-explore.md": "c81aff53e081f987463f8b9e0ab396becc8025be5bde52ebf480f3389e378aff",
	"gortex-extract-function.md": "cca40211532f7883a9ef882dcfd8889a01bc431bd4cf1c86e03c374608359acf", "gortex-fix-all.md": "3401163345332c89742bd4493b42081a230d35e0ea520dca727b2178f1cfc19c",
	"gortex-guide.md": "f53403043ee232f487031dde3c7a845b9c9c6066a2075eb26e629ef898c809ac", "gortex-impact.md": "810265956680292a24f813ab8b627288782afe6c5c9a948d2519e1339bbeeba9",
	"gortex-incident-investigation.md": "ffa1d998e413d2baecbf6f998de82fe3dc3aeb13fa1c6e62554bab76cb26614f", "gortex-onboarding.md": "7965dc26cd556a3a3924748d3b3658bd6616818ea1e3f9c8b5803a403c9dddf9",
	"gortex-pr-review-agent.md": "3a69060d52fccaba694b9176a562557777d39178f3d685ea89865b4716b86cc8", "gortex-pr-review.md": "125a5197b8554579ae1d0e7f4b74051499d4021a936b821812ba1d5fc387ac8d",
	"gortex-quality-audit.md": "0358173a3c822c0f00b21ec7d673edcf6f96e2bd6fa0c76183dfdf586e719724", "gortex-refactor.md": "0cb9486f1372626cc84ae154e7b16a7c70eb68e888f15b5c291cfa21fe5d8a3d",
	"gortex-rename.md": "e9bfc1f4213e7334e64bf7c495f4486bab978a9dab8e868c67683fa2f451796b", "gortex-safe-edit.md": "c262e1ca08752d5f97cf9109eafa0ffe458a7196af7a8154a708816abb093833",
}

var preWorktreeSubAgentHashes = map[string]string{
	"gortex-impact.md": "031565a4ff5cf6731a6145d883b9be419f529179631b18e400b3383bd4878ee1",
	"gortex-search.md": "881631c1588c304c95b25ae1c74259e6afc5b9682eec034d42abcc3c74edb996",
}

func artifactHash(content []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(content))
}

func isShippedAgentArtifact(existing []byte, current string, migrationHashes ...string) bool {
	if string(existing) == current {
		return true
	}
	hash := artifactHash(existing)
	for _, migrationHash := range migrationHashes {
		if migrationHash != "" && hash == migrationHash {
			return true
		}
	}
	return false
}

func writeAgentArtifact(w io.Writer, path, current string, migrationHashes []string, opts agents.ApplyOpts) (agents.FileAction, error) {
	existing, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return agents.WriteIfNotExists(w, path, current, opts)
	}
	if err != nil {
		return agents.FileAction{}, fmt.Errorf("read %s: %w", path, err)
	}
	if string(existing) == current {
		return agents.FileAction{Path: path, Action: agents.ActionSkip, Reason: "unchanged"}, nil
	}
	if isShippedAgentArtifact(existing, current, migrationHashes...) {
		return agents.WriteOwnedFile(w, path, current, opts)
	}
	logWarn(w, "keeping customised agent artifact %s", path)
	return agents.FileAction{Path: path, Action: agents.ActionSkip, Reason: "customised"}, nil
}

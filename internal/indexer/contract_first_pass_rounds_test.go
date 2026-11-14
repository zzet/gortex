package indexer

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestContractFirstPassRoundSize(t *testing.T) {
	for _, row := range []struct {
		files, rounds, want int
	}{
		{0, contractFirstPassRounds, contractRoundFiles},
		{1, contractFirstPassRounds, contractRoundFiles},
		{contractRoundFiles * contractFirstPassRounds, contractFirstPassRounds, contractRoundFiles},
		{contractRoundFiles*contractFirstPassRounds + 1, contractFirstPassRounds, contractRoundFiles + 1},
		{218_000, contractFirstPassRounds, 852},
		{200, 2, 100},
		{200, 0, contractRoundFiles},
	} {
		got := contractFirstPassRoundSize(row.files, row.rounds)
		require.Equal(t, row.want, got, "%d files, %d rounds", row.files, row.rounds)
		require.GreaterOrEqual(t, got, contractRoundFiles, "a round is never smaller than contractRoundFiles")
		if row.rounds > 0 && row.files > contractRoundFiles*row.rounds {
			require.LessOrEqual(t, (row.files+got-1)/got, row.rounds, "the batch fits its round bound")
		}
	}
}

// contractRoundsFixture is the job fixture's three contract files and 197
// plain Go files: 200 census files, four rounds of contractRoundFiles.
func contractRoundsFixture() map[string]string {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	for i := range 197 {
		files[fmt.Sprintf("pkg%03d.go", i)] = fmt.Sprintf("package fixture\nfunc helper%03d() int { return %d }\n", i, i)
	}
	return files
}

func runContractRoundsJob(t *testing.T, files map[string]string, want []string, hooks *contractConvergenceHooks) ContractFollowupReport {
	t.Helper()
	f := newContractJobFixture(t, files)
	hooks.roundWait = 5 * time.Second
	report, err := f.job(hooks, contractJobDeadline(time.Minute))
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, want, contractIDs(f.contracts()), "the job equals a cold build")
	return report
}

// A batch larger than its round bound allows takes fewer, larger rounds, each
// with its own full scan, and publishes what a cold build does.
func TestContractJobFirstPassTakesBoundedRounds(t *testing.T) {
	files := contractRoundsFixture()
	want := contractIDs(coldContracts(t, files))
	fixed := runContractRoundsJob(t, files, want, &contractConvergenceHooks{})
	bounded := runContractRoundsJob(t, files, want, &contractConvergenceHooks{firstPassRounds: 2})
	require.Equal(t, 200, fixed.Files, "every file is extracted once")
	require.Equal(t, 200, bounded.Files, "every file is extracted once")
	require.Equal(t, 4, fixed.Rounds, "four rounds of contractRoundFiles")
	require.Equal(t, 2, bounded.Rounds, "two rounds of 100 files")
}

// A round larger than contractRoundFiles whose files outgrow one evidence
// store fails with the row limit. The job does not fail: that round's files
// and the rest of the batch take rounds of contractRoundFiles, which fit.
func TestContractJobLargeRoundOverEvidenceLimitTakesSmallerRounds(t *testing.T) {
	files := contractRoundsFixture()
	want := contractIDs(coldContracts(t, files))
	report := runContractRoundsJob(t, files, want, &contractConvergenceHooks{firstPassRounds: 2, evidenceFiles: contractRoundFiles})
	require.Equal(t, 5, report.Rounds, "the failed round of 100, then four of contractRoundFiles")
	require.Equal(t, 200+contractRoundFiles, report.Files, "the failed round's extractions are repeated")
}

// A failed round halves, not straight to contractRoundFiles: when rounds of
// half the size fit, the rest of the batch takes those.
func TestContractJobRoundOverEvidenceLimitHalves(t *testing.T) {
	files := contractRoundsFixture()
	want := contractIDs(coldContracts(t, files))
	report := runContractRoundsJob(t, files, want, &contractConvergenceHooks{firstPassRounds: 1, evidenceFiles: 150})
	require.Equal(t, 3, report.Rounds, "the failed round of 200, then two of 100")
	require.Equal(t, 200+150, report.Files, "the failed round's extractions are repeated")
}

// A round of contractRoundFiles that fails with the row limit still fails the
// job, as before: there is no smaller round to fall back to.
func TestContractJobRoundOfContractRoundFilesOverEvidenceLimitFails(t *testing.T) {
	files := contractRoundsFixture()
	f := newContractJobFixture(t, files)
	_, err := f.job(&contractConvergenceHooks{evidenceFiles: 10, roundWait: 5 * time.Second}, contractJobDeadline(time.Minute))
	require.ErrorIs(t, err, graph.ErrContractProjectionLimit)
}

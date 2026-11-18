package indexer

import (
	"context"
	"errors"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
)

// mutationAdmissionSampler is the existing checked whole-checkout sampler.
// A zero since preserves Sample's unconditional fresh read. A positive bound
// preserves SampleSince's sharing only with cuts that began after that bound.
type mutationAdmissionSampler interface {
	Sample(context.Context) (gitstate.DirtySnapshot, error)
	SampleSince(context.Context, time.Time) (gitstate.DirtySnapshot, error)
}

// sampleMutationAdmission retries only a torn PREWRITE observation once. It
// neither retries validation nor changes any root/HEAD/route/fingerprint/CAS
// check after the cut. It is not used by builders or postwrite ticket capture.
func sampleMutationAdmission(ctx context.Context, sampler mutationAdmissionSampler, since time.Time) (gitstate.DirtySnapshot, error) {
	var sample gitstate.DirtySnapshot
	var err error
	if since.IsZero() {
		sample, err = sampler.Sample(ctx)
	} else {
		sample, err = sampler.SampleSince(ctx, since)
	}
	if err == nil || !errors.Is(err, gitstate.ErrDirtyUnavailable) || !errors.Is(err, gitstate.ErrDirtyMoved) || ctx.Err() != nil {
		return sample, err
	}
	// Taken AFTER the failed call returned. SampleSince cannot share a cached
	// success from before that cut, even when its fingerprint happens to match.
	retrySince := time.Now()
	return sampler.SampleSince(ctx, retrySince)
}

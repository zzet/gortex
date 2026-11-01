package indexer

import (
	"context"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

// Publication must mean the native worker has returned its allowance. A caller
// often starts another observation immediately; announcing completion before
// permit return can spuriously exhaust both slots under parallel tracking.
func TestIdentityObservationPublishesOnlyAfterPermitReturn(t *testing.T) {
	root := t.TempDir()
	for _, operation := range []string{"stat", "naming"} {
		t.Run(operation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var allowance prefixStatAllowance
				allowance.once.Do(func() { allowance.slots = make(chan struct{}, 2) })
				nativeDone := make(chan struct{})
				// Temporarily intercept this worker's token so its release must
				// wait for the test. This makes publication ordering observable
				// without scheduler timing, polling or an extra production hook.
				finishNative := func() {
					<-allowance.slots
					close(nativeDone)
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				result := make(chan error, 1)
				if operation == "stat" {
					allowance.stat = func(path string) (os.FileInfo, error) {
						info, err := os.Stat(path)
						finishNative()
						return info, err
					}
					go func() { result <- allowance.observe(ctx, root).err }()
				} else {
					go func() {
						_, err := allowance.observeNaming(ctx, root, func(context.Context, string) trackNamingFacts {
							finishNative()
							return trackNamingFacts{canonical: root}
						})
						result <- err
					}()
				}
				<-nativeDone
				synctest.Wait()
				var publishedEarly bool
				var err error
				select {
				case err = <-result:
					publishedEarly = true
				default:
				}
				// Always let the worker finish before assertions, including RED.
				allowance.slots <- struct{}{}
				synctest.Wait()
				if !publishedEarly {
					err = <-result
				}
				require.NoError(t, err)
				require.Zero(t, len(allowance.slots))
				require.False(t, publishedEarly, "completed identity result escaped before its permit was returned")
			})
		})
	}
}

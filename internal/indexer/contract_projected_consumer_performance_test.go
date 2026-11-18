//go:build performance

package indexer

import (
	"testing"
	"time"
)

// Preserve the original shared consumer deadline, including baseline repair,
// scratch cleanup and the later accepted attachment, outside ordinary CI.
func TestPerformanceContractProjectedConsumerMixedAttachmentSharedBudget(t *testing.T) {
	testContractProjectedConsumerPublishesMixedAcceptedAttachment(t, 5*time.Second, false)
}

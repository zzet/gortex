package store_sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestContractAttachmentExactProofAndIndexedPhysicalDiscovery(t *testing.T) {
	s := openCatalogStore(t)
	target := attachmentBatchTarget(t, s, "repo")
	if _, err := s.PublishContractAttachmentsWithInputsContext(context.Background(), []ContractAttachmentPublishTarget{target}, 2); err != nil {
		t.Fatal(err)
	}
	w := target.Witnesses[0]
	published, err := s.ContractInputIdentityPublishedContext(context.Background(), w)
	if err != nil || !published {
		t.Fatalf("physical identity not discovered: %v %v", published, err)
	}
	foreign := w
	foreign.GenerationID++
	published, err = s.ContractInputIdentityPublishedContext(context.Background(), foreign)
	if err != nil || published {
		t.Fatalf("different generation borrowed publication: %v %v", published, err)
	}
	header, proof, err := s.ContractAttachmentProofContext(context.Background(), attachmentKey(target.Expected))
	if err != nil || header == nil || len(proof) != 1 || proof[0].GenerationID != w.GenerationID {
		t.Fatalf("exact proof: %+v %+v %v", header, proof, err)
	}
	if err := s.Catalog().SetViewGenerationState(context.Background(), header.PayloadGeneration, ViewGenerationRetiring, ViewGenerationReady); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.ContractAttachmentProofContext(context.Background(), attachmentKey(target.Expected))
	if !errors.Is(err, graph.ErrContractPredecessorRetired) {
		t.Fatalf("retiring exact payload accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ContractInputIdentityPublishedContext(ctx, w); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

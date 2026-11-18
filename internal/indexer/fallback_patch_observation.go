package indexer

// fallbackPatchObservation observes the real successful legacy extraction,
// commit and search boundaries. It is local to one existing marker batch;
// unknown/failed/skipped paths retain incomplete evidence.
type fallbackPatchObservation struct {
	stamps *PublicationStamps
	active canonicalLapSpan
}

func beginFallbackPatchObservation(batch *reparsePendingEnrichmentBatch) *fallbackPatchObservation {
	if batch == nil || !batch.deferResolverCatchup || batch.publication == nil {
		return nil
	}
	stamps := batch.publicationStamps()
	return &fallbackPatchObservation{stamps: stamps, active: stamps.beginPatchLap(patchParse)}
}

func (o *fallbackPatchObservation) next(lap canonicalPatchLap) {
	if o != nil {
		o.active.end()
		o.active = o.stamps.beginPatchLap(lap)
	}
}

func (o *fallbackPatchObservation) finish(complete bool) {
	if o != nil {
		o.active.end()
		if !complete {
			o.stamps.incompletePatchLaps()
		}
	}
}

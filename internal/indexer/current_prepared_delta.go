package indexer

// currentPreparedDelta keeps the probe and its owned parse paired. A refresh
// must never return newer source alongside the older probe's fingerprints.
func (idx *Indexer) currentPreparedDelta(
	filePath string,
	probe fileDeltaProbe,
	tryOnly bool,
	repair *ReadRepairObservation,
) (fileDeltaProbe, *preparedExtraction, bool) {
	prepared, ok := idx.takePreparedRefresh(filePath)
	return probe, prepared, ok
}

package indexer

import (
	"bytes"
	"path/filepath"
)

// currentPreparedDelta keeps the probe and its owned parse paired. On one
// newer save it replaces the obsolete parse through ordinary shared admission,
// so the caller can use the same staged apply instead of the combined fallback.
// Another movement, read failure, unsupported extraction or admission pressure
// keeps the conservative fallback. No retry loop or stale-byte reuse is allowed.
func (idx *Indexer) currentPreparedDelta(
	filePath string,
	probe fileDeltaProbe,
	tryOnly bool,
	repair *ReadRepairObservation,
) (current fileDeltaProbe, prepared *preparedExtraction, reprepared, ok bool) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return probe, nil, false, false
	}
	prior, found := idx.takePreparedSnapshot(absPath)
	if !found {
		return probe, nil, false, false
	}
	source, version, err := idx.readFileWithVersion(absPath)
	if err != nil {
		prior.release()
		return probe, nil, false, false
	}
	source = idx.transforms.run(prior.relPath, source)
	if bytes.Equal(source, prior.src) {
		prior.readVersion = version
		return probe, prior, false, true
	}
	// The old parse and its shared admission belong to the old bytes. Release
	// both before asking for the newer extraction, including a larger input.
	prior.release()
	fresh, found, busy := idx.prepareFileDeltaWithAdmissionObserved(absPath, tryOnly, repair)
	if !found || busy {
		return fresh, nil, true, false
	}
	// The second parse is not authority to publish: retain the existing current
	// byte/readVersion check and let the normal commit receipt fence it again.
	prepared, ok = idx.takePreparedRefresh(absPath)
	return fresh, prepared, true, ok
}

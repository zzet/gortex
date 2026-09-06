package indexer

import "testing"

// BenchmarkAdmitWalkEntryOrders compares all gates on the same corpus and
// process. known_type models the filesystem walk, whose DirEntry.Info already
// supplies the mode; the historical order exists only in this benchmark.
func BenchmarkAdmitWalkEntryOrders(b *testing.B) {
	for _, pop := range benchAdmitPopulations {
		b.Run(pop.name, func(b *testing.B) {
			root := b.TempDir()
			paths := writeBenchPopulation(b, root, pop.dir, pop.names)
			idx := newAdmissionTestIndexer(b, root)
			for _, order := range []struct {
				name  string
				admit func(string) walkAdmission
			}{
				{"language_first", func(path string) walkAdmission {
					lang, ok := idx.effectiveLanguage(path, nil)
					if !ok {
						return walkAdmission{}
					}
					if idx.shouldExclude(path, root, false) {
						return walkAdmission{lang: lang}
					}
					return walkAdmission{lang: lang, admit: true}
				}},
				{"exclude_first", func(path string) walkAdmission {
					return idx.admitWalkEntry(root, path, benchFileSize, false)
				}},
				{"known_type", func(path string) walkAdmission {
					return idx.admitWalkFileKnownType(root, path, benchFileSize, 0)
				}},
			} {
				b.Run(order.name, func(b *testing.B) {
					if adm := order.admit(paths[0]); adm.admit != pop.wantAdmit {
						b.Fatalf("admitted=%v, want %v (%+v)", adm.admit, pop.wantAdmit, adm)
					}
					b.ReportAllocs()
					i := 0
					for b.Loop() {
						order.admit(paths[i%len(paths)])
						i++
					}
				})
			}
		})
	}
}

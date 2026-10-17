package graphview

// GenerationID is the published generation the layer reads, so a reader
// composing layers can tell which generation each one is.
func (l *GenerationLayer) GenerationID() int64 {
	if l == nil || l.handle == nil {
		return 0
	}
	return l.handle.ViewGeneration()
}

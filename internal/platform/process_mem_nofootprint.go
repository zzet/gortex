//go:build !(darwin && cgo)

package platform

func physFootprintBytes() (current, peak uint64) { return 0, 0 }

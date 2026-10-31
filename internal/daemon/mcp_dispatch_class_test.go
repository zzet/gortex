package daemon

import "testing"

func TestDispatchClassIncludesEveryDurableWriter(t *testing.T) {
	for name := range ToolEffects {
		if !IsMutating(name) {
			continue
		}
		class, known := ToolDispatchClass(name)
		if !known || class != MCPDispatchMutating {
			t.Errorf("durable writer %q class=%s known=%v", name, class, known)
		}
	}
}

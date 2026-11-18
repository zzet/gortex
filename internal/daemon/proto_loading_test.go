package daemon

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrackedRepoLoadingJSONCompatibility(t *testing.T) {
	encoded, err := json.Marshal(TrackedRepoStatus{Loading: true, Unloaded: true})
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"loading":true`)
	// Older clients still see Unloaded when they ignore the additive field.
	var oldClient struct {
		Unloaded bool `json:"unloaded"`
	}
	require.NoError(t, json.Unmarshal(encoded, &oldClient))
	assert.True(t, oldClient.Unloaded)
	encoded, err = json.Marshal(TrackedRepoStatus{})
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), `"loading"`)
}

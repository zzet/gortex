package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// A class written on one line shares its line with its methods; heritage
// still belongs to the class, not to the method declared beside it.
func TestPHPHeritageOwnerOnOneLineClass(t *testing.T) {
	src := "<?php namespace App;\nclass Store extends Base implements Exportable { public function check() { return 1; } }"
	result, err := NewPHPExtractor().Extract("s.php", []byte(src))
	require.NoError(t, err)
	owners := map[graph.EdgeKind][]string{}
	for _, e := range result.Edges {
		if e.Kind == graph.EdgeExtends || e.Kind == graph.EdgeImplements {
			owners[e.Kind] = append(owners[e.Kind], e.From)
		}
	}
	assert.Equal(t, []string{"s.php::Store"}, owners[graph.EdgeExtends])
	assert.Equal(t, []string{"s.php::Store"}, owners[graph.EdgeImplements])
}

package nuxtimports

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGeneratedAppBindings(t *testing.T) {
	root := filepath.Join(t.TempDir(), "build") // skip rules apply below, not to the checkout itself
	generated := filepath.Join(root, ".nuxt")
	require.NoError(t, os.MkdirAll(generated, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(generated, "imports.d.ts"), []byte(`export { useRedirect, original as renamed } from '../app/composables/auth';
export { ignored } from 'package';
export { internal } from '#app/composables';
export { escaped } from '../../outside';
export { duplicate } from '../app/one';
export { duplicate } from '../app/two';
export { type TypeOnly } from '../app/types';
`), 0o644))
	coll := Load(root)
	require.Equal(t, Binding{File: "app/composables/auth", Name: "useRedirect"}, coll.Resolve("app/pages/index.vue", "useRedirect"))
	require.Equal(t, Binding{File: "app/composables/auth", Name: "original"}, coll.Resolve("app/page.ts", "renamed"))
	for _, name := range []string{"ignored", "internal", "escaped", "duplicate", "TypeOnly"} {
		require.Empty(t, coll.Resolve("app/page.ts", name))
	}
	require.Empty(t, coll.Resolve("server/api/redirect.ts", "useRedirect"))
	require.Empty(t, coll.Resolve("layers/shop/server/api/items.ts", "useRedirect"))
	require.NotEmpty(t, coll.Resolve("app/pages/server/index.vue", "useRedirect"))
	require.NotEmpty(t, coll.Resolve("app/components/server/Status.vue", "useRedirect"))
	require.Empty(t, coll.Resolve("layers/shop/nuxt.config.ts", "useRedirect"))
	require.Empty(t, coll.Resolve(".nuxt/imports.d.ts", "useRedirect"))
}

func TestNestedNuxtRootsDoNotLeak(t *testing.T) {
	root := t.TempDir()
	generated := filepath.Join(root, "apps/shop/.nuxt")
	require.NoError(t, os.MkdirAll(generated, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(generated, "imports.d.ts"), []byte(`export { useShop } from '../app/composables/shop';`), 0o644))
	unprepared := filepath.Join(root, "apps/shop/apps/store")
	require.NoError(t, os.MkdirAll(unprepared, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(unprepared, "nuxt.config.mts"), []byte("export default {}"), 0o644))
	coll := Load(root)
	require.Equal(t, Binding{File: "apps/shop/app/composables/shop", Name: "useShop"}, coll.Resolve("apps/shop/app/page.vue", "useShop"))
	require.Empty(t, coll.Resolve("apps/admin/app/page.vue", "useShop"))
	require.Empty(t, coll.Resolve("apps/shop/apps/store/app/index.vue", "useShop"))
	require.Empty(t, coll.Resolve("apps/shop/server/api.ts", "useShop"))
	require.Nil(t, Load(t.TempDir()))
}

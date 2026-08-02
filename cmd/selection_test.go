package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveSelectionPath(t *testing.T) {
	t.Run("default when neither set", func(t *testing.T) {
		p, err := resolveSelectionPath("", "")
		require.NoError(t, err)
		assert.Equal(t, defaultSelectionPath, p)
	})

	t.Run("explicit path passthrough", func(t *testing.T) {
		p, err := resolveSelectionPath("", "some/where.selection")
		require.NoError(t, err)
		assert.Equal(t, "some/where.selection", p)
	})

	t.Run("name maps into registry", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
		p, err := resolveSelectionPath("payments", "")
		require.NoError(t, err)
		assert.Equal(t, filepath.FromSlash("/tmp/xdg/goldfinger/selections/payments.json"), p)
	})

	t.Run("name and path are mutually exclusive", func(t *testing.T) {
		_, err := resolveSelectionPath("payments", "some/path")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mutually exclusive")
	})

	t.Run("rejects unsafe names", func(t *testing.T) {
		for _, bad := range []string{"../escape", "a/b", "..", "."} {
			_, err := resolveSelectionPath(bad, "")
			require.Error(t, err, bad)
			assert.Contains(t, err.Error(), "invalid selection name")
		}
	})
}

package main

import (
	"testing"
	"time"

	"github.com/redscaresu/goldfinger/models"
	"github.com/redscaresu/goldfinger/selection"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectionsCommand(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	t.Run("empty registry", func(t *testing.T) {
		out, err := executeCmd(t, "", "selections")
		require.NoError(t, err)
		assert.Contains(t, out, "no named selections")
	})

	// Seed one named selection.
	p, err := selection.PathForName("platform")
	require.NoError(t, err)
	require.NoError(t, selection.Write(p, models.Selection{
		Version:    models.SelectionVersion,
		Owner:      "acme",
		ResolvedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		Repos:      []models.Repo{{Owner: "acme", Name: "a"}, {Owner: "acme", Name: "b"}},
	}))

	t.Run("lists the selection with a summary", func(t *testing.T) {
		out, err := executeCmd(t, "", "selections")
		require.NoError(t, err)
		assert.Contains(t, out, "platform")
		assert.Contains(t, out, "acme")
		assert.Contains(t, out, "2") // repo count
		assert.Contains(t, out, "2026-08-01")
	})
}

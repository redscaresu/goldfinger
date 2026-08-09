package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestSelectionsJSON(t *testing.T) {
	t.Run("empty registry is an empty list, not an error", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		var buf bytes.Buffer
		require.NoError(t, emitSelectionsJSON(&buf, nil, false))
		var rep selectionsReport
		require.NoError(t, json.Unmarshal(buf.Bytes(), &rep))
		assert.Equal(t, selectionsReportVersion, rep.Version)
		assert.NotNil(t, rep.Selections)
		assert.Empty(t, rep.Selections)
	})

	t.Run("readable and unreadable entries", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())

		good, err := selection.PathForName("platform")
		require.NoError(t, err)
		require.NoError(t, selection.Write(good, models.Selection{
			Version:    models.SelectionVersion,
			Owner:      "acme",
			ResolvedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			Repos:      []models.Repo{{Owner: "acme", Name: "a"}, {Owner: "acme", Name: "b"}},
		}))
		// A malformed entry must be represented inline with an error, not dropped.
		dir, err := selection.Dir()
		require.NoError(t, err)
		bad := filepath.Join(dir, "broken.json")
		require.NoError(t, os.WriteFile(bad, []byte("{not json"), 0o644))

		names, err := selection.Names()
		require.NoError(t, err)

		var buf bytes.Buffer
		require.NoError(t, emitSelectionsJSON(&buf, names, false))
		var rep selectionsReport
		require.NoError(t, json.Unmarshal(buf.Bytes(), &rep))

		byName := map[string]selectionEntryJSON{}
		for _, e := range rep.Selections {
			byName[e.Name] = e
		}
		require.Contains(t, byName, "platform")
		assert.Equal(t, "acme", byName["platform"].Owner)
		require.NotNil(t, byName["platform"].RepoCount, "a readable entry always carries repoCount")
		assert.Equal(t, 2, *byName["platform"].RepoCount)
		assert.Empty(t, byName["platform"].Error)
		// A readable entry carries the repo-set digest (issue #48 WS6) so an agent
		// can compare selections without reading each lockfile; it matches Digest.
		_, wantDigest := selection.Digest(models.Selection{
			Repos: []models.Repo{{Owner: "acme", Name: "a"}, {Owner: "acme", Name: "b"}},
		})
		assert.Equal(t, wantDigest, byName["platform"].Digest)

		require.Contains(t, byName, "broken")
		assert.NotEmpty(t, byName["broken"].Error, "unreadable entry carries an error, not dropped")
		assert.Nil(t, byName["broken"].RepoCount, "an unreadable entry has null repoCount, distinguishing it from a zero-repo selection")
		assert.Empty(t, byName["broken"].Digest, "an unreadable entry has no digest — it carries error instead")
	})
}

func TestRunSelectionsQuiet(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	good, err := selection.PathForName("platform")
	require.NoError(t, err)
	require.NoError(t, selection.Write(good, models.Selection{
		Version:    models.SelectionVersion,
		Owner:      "acme",
		ResolvedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		Repos:      []models.Repo{{Owner: "acme", Name: "a"}},
	}))
	names := []string{"platform"}

	t.Run("non-json emits nothing", func(t *testing.T) {
		var out, errOut bytes.Buffer
		require.NoError(t, runSelections(names, selectionsOptions{quiet: true}, &out, &errOut))
		assert.Empty(t, out.String())
		assert.Empty(t, errOut.String())
	})

	t.Run("json emits report only", func(t *testing.T) {
		var out, errOut bytes.Buffer
		require.NoError(t, runSelections(names, selectionsOptions{asJSON: true, quiet: true}, &out, &errOut))
		var rep selectionsReport
		require.NoError(t, json.Unmarshal(out.Bytes(), &rep))
		require.Len(t, rep.Selections, 1)
		assert.Equal(t, "platform", rep.Selections[0].Name)
		assert.Empty(t, errOut.String())
	})
}

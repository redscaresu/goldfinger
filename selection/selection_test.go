package selection

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleSelection() models.Selection {
	return models.Selection{
		Version:    models.SelectionVersion,
		Owner:      "redscaresu",
		OwnerType:  "User",
		Filter:     models.SelectionFilter{Topics: []string{"platform"}},
		ResolvedAt: time.Date(2026, 8, 1, 15, 0, 0, 0, time.UTC),
		Tool:       "goldfinger test",
		Repos: []models.Repo{
			{Owner: "redscaresu", Name: "goldfinger", DefaultBranch: "main", Topics: []string{"platform"}},
			{Owner: "redscaresu", Name: "simpleAPI", DefaultBranch: "master"},
		},
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goldfinger.selection")
	want := sampleSelection()

	require.NoError(t, Write(path, want))

	got, err := Read(path)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestReadMissingFile(t *testing.T) {
	_, err := Read(filepath.Join(t.TempDir(), "nope.selection"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "goldfinger select")
}

func TestReadUnsupportedVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goldfinger.selection")
	require.NoError(t, os.WriteFile(path, []byte(`{"version":999,"owner":"x"}`), 0o644))

	_, err := Read(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version")
}

func TestReadCorruptJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goldfinger.selection")
	require.NoError(t, os.WriteFile(path, []byte(`not json`), 0o644))

	_, err := Read(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse selection")
}

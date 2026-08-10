package selection

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleSelection() models.Selection {
	return models.Selection{
		Version:         models.SelectionVersion,
		Owner:           "redscaresu",
		OwnerType:       "User",
		Filter:          models.SelectionFilter{Topics: []string{"platform"}},
		ResolvedAt:      time.Date(2026, 8, 1, 15, 0, 0, 0, time.UTC),
		Tool:            "goldfinger test",
		BranchesChecked: []string{"dev"},
		Repos: []models.Repo{
			{Owner: "redscaresu", Name: "goldfinger", DefaultBranch: "main", Topics: []string{"platform"}, BranchPresence: map[string]bool{"dev": true}},
			{Owner: "redscaresu", Name: "simpleAPI", DefaultBranch: "master", BranchPresence: map[string]bool{"dev": false}},
		},
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goldfinger.selection")
	want := sampleSelection()

	require.NoError(t, Write(path, want, WriteOptions{Overwrite: true}))

	got, err := Read(path)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestWriteNoOverwriteRefusesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goldfinger.selection")
	first := sampleSelection()
	require.NoError(t, Write(path, first, WriteOptions{Overwrite: false}))

	// A second create-or-fail write must refuse rather than clobber, and the
	// error must unwrap to fs.ErrExist so callers can detect the collision.
	second := sampleSelection()
	second.Owner = "someone-else"
	err := Write(path, second, WriteOptions{Overwrite: false})
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrExist)

	// The original bytes are untouched — the loser never wrote over the winner.
	got, err := Read(path)
	require.NoError(t, err)
	assert.Equal(t, first.Owner, got.Owner)

	// No temp files linger in the directory after a refused write.
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp", "temp file must not linger")
	}
}

func TestWriteConcurrentCreateOrFailExactlyOneWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goldfinger.selection")
	const writers = 8
	var wg sync.WaitGroup
	results := make([]error, writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sel := sampleSelection()
			sel.Owner = fmt.Sprintf("writer-%d", i)
			<-start // line them all up so the link race is real
			results[i] = Write(path, sel, WriteOptions{Overwrite: false})
		}(i)
	}
	close(start)
	wg.Wait()

	wins := 0
	for _, err := range results {
		if err == nil {
			wins++
		} else {
			assert.ErrorIs(t, err, os.ErrExist)
		}
	}
	assert.Equal(t, 1, wins, "exactly one concurrent create-or-fail writer succeeds")

	// The file is one intact writer's lockfile, never a torn blend of several.
	got, err := Read(path)
	require.NoError(t, err)
	assert.Regexp(t, `^writer-\d$`, got.Owner)
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

func TestReadMigratesV1(t *testing.T) {
	// A v1 lockfile predates branch-presence metadata; Read must accept it and
	// leave branch facts empty so they read back as "unknown" — never guessed.
	path := filepath.Join(t.TempDir(), "goldfinger.selection")
	v1 := `{
  "version": 1,
  "owner": "redscaresu",
  "ownerType": "User",
  "filter": { "topics": ["platform"] },
  "resolvedAt": "2026-08-01T15:00:00Z",
  "tool": "goldfinger v1",
  "repos": [
    { "owner": "redscaresu", "name": "goldfinger", "cloneURL": "https://github.com/redscaresu/goldfinger.git", "defaultBranch": "main", "topics": ["platform"] }
  ]
}`
	require.NoError(t, os.WriteFile(path, []byte(v1), 0o644))

	got, err := Read(path)
	require.NoError(t, err)
	assert.Equal(t, 1, got.Version)
	assert.Empty(t, got.BranchesChecked)
	require.Len(t, got.Repos, 1)
	assert.Nil(t, got.Repos[0].BranchPresence)

	// A branch that isn't the default reads as unknown (not guessed).
	has, known := got.Repos[0].RecordedBranch("dev")
	assert.False(t, known)
	assert.False(t, has)
}

func TestNamedSelectionRegistry(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Empty registry: no names, no error.
	names, err := Names()
	require.NoError(t, err)
	assert.Empty(t, names)

	// Write two named selections via their registry paths.
	for _, n := range []string{"payments", "platform"} {
		p, err := PathForName(n)
		require.NoError(t, err)
		sel := sampleSelection()
		sel.Owner = n
		require.NoError(t, Write(p, sel, WriteOptions{Overwrite: true})) // Write creates the registry dir
	}

	names, err = Names()
	require.NoError(t, err)
	assert.Equal(t, []string{"payments", "platform"}, names, "names are sorted")

	// Round-trip a named selection by name.
	p, err := PathForName("payments")
	require.NoError(t, err)
	got, err := Read(p)
	require.NoError(t, err)
	assert.Equal(t, "payments", got.Owner)
}

func TestDirHonoursXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	dir, err := Dir()
	require.NoError(t, err)
	assert.Equal(t, "/tmp/xdg/goldfinger/selections", dir)
}

func TestDirFallsBackToHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	dir, err := Dir()
	require.NoError(t, err)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".config", "goldfinger", "selections"), dir)
}

func TestReadCorruptJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goldfinger.selection")
	require.NoError(t, os.WriteFile(path, []byte(`not json`), 0o644))

	_, err := Read(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse selection")
}

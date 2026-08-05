package mirror

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func userSelection() models.Selection {
	return models.Selection{
		Version:   models.SelectionVersion,
		Owner:     "redscaresu",
		OwnerType: models.OwnerUser,
		Repos: []models.Repo{
			{Owner: "redscaresu", Name: "goldfinger"},
			{Owner: "redscaresu", Name: "simpleAPI"},
		},
	}
}

// capture records the last invocation the Runner received.
type capture struct {
	name string
	args []string
	env  []string
}

func (c *capture) run(_ context.Context, name string, args, env []string) error {
	c.name, c.args, c.env = name, args, env
	return nil
}

func TestMirrorInvocation(t *testing.T) {
	var cap capture
	err := Mirror(context.Background(), cap.run, userSelection(), "secret-token",
		Options{Workspace: "/tmp/ws", Concurrency: 20, CloneDepth: 1})
	require.NoError(t, err)

	assert.Equal(t, "ghorg", cap.name)
	assert.Equal(t, "clone", cap.args[0])
	assert.Equal(t, "redscaresu", cap.args[1])
	assert.Contains(t, cap.args, "--clone-type=user")
	assert.Contains(t, cap.args, "--path=/tmp/ws")
	assert.Contains(t, cap.args, "--concurrency=20")
	assert.Contains(t, cap.args, "--clone-depth=1")

	// The token must travel in the env, never argv.
	assert.NotContains(t, strings.Join(cap.args, " "), "secret-token")
	assert.Contains(t, cap.env, tokenEnv+"=secret-token")

	// --target-repos-path points at a file listing the selected repo names.
	var namesPath string
	for _, a := range cap.args {
		if strings.HasPrefix(a, "--target-repos-path=") {
			namesPath = strings.TrimPrefix(a, "--target-repos-path=")
		}
	}
	require.NotEmpty(t, namesPath)
	// File is cleaned up after Mirror returns.
	_, statErr := os.Stat(namesPath)
	assert.True(t, os.IsNotExist(statErr), "names file should be removed after Mirror returns")
}

func TestMirrorWritesRepoNames(t *testing.T) {
	var gotNames string
	run := func(_ context.Context, _ string, args, _ []string) error {
		for _, a := range args {
			if strings.HasPrefix(a, "--target-repos-path=") {
				data, err := os.ReadFile(strings.TrimPrefix(a, "--target-repos-path="))
				require.NoError(t, err)
				gotNames = string(data)
			}
		}
		return nil
	}
	require.NoError(t, Mirror(context.Background(), run, userSelection(), "tok", Options{}))
	assert.Equal(t, "goldfinger\nsimpleAPI\n", gotNames)
}

func TestMirrorOverridesExistingToken(t *testing.T) {
	t.Setenv(tokenEnv, "runner-default-token")
	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "our-token", Options{}))

	var vals []string
	for _, e := range cap.env {
		if strings.HasPrefix(e, tokenEnv+"=") {
			vals = append(vals, e)
		}
	}
	require.Len(t, vals, 1, "exactly one token entry should reach the child")
	assert.Equal(t, tokenEnv+"=our-token", vals[0])
}

func TestMirrorNeutralisesAmbientConfig(t *testing.T) {
	// Ambient set-narrowing/pruning GHORG_* vars must not reach ghorg, and an
	// empty ghorgignore must be forced so no host ignore file can drop repos.
	t.Setenv("GHORG_TOPICS", "should-be-stripped")
	t.Setenv("GHORG_MATCH_REGEX", "^keep-")
	t.Setenv("GHORG_SKIP_ARCHIVED", "true")
	t.Setenv("GHORG_PRUNE_NO_CONFIRM", "true")
	t.Setenv("GHORG_IGNORE_PATH", "/host/ghorgignore")

	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "tok", Options{}))

	for _, e := range cap.env {
		for _, banned := range ambientGhorgEnv {
			assert.NotContains(t, e, banned+"=", "ambient ghorg config var %s must be stripped", banned)
		}
	}

	var ignorePath string
	for _, a := range cap.args {
		if strings.HasPrefix(a, "--ghorgignore-path=") {
			ignorePath = strings.TrimPrefix(a, "--ghorgignore-path=")
		}
	}
	require.NotEmpty(t, ignorePath, "mirror must force an explicit --ghorgignore-path")
	assert.NotEqual(t, "/host/ghorgignore", ignorePath, "must not use the host ghorgignore")
	// File is cleaned up after Mirror returns.
	_, statErr := os.Stat(ignorePath)
	assert.True(t, os.IsNotExist(statErr), "temp ghorgignore should be removed after Mirror returns")
}

func TestMirrorStripsSourcePATFromChildEnv(t *testing.T) {
	t.Setenv(models.TokenEnvVar, "raw-pat") // operator's exported GOLD_FINGER_PAT
	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "mapped-token", Options{}))

	for _, e := range cap.env {
		assert.NotContains(t, e, models.TokenEnvVar+"=", "source PAT var must not reach the child")
		assert.NotContains(t, e, "raw-pat", "raw PAT value must not reach the child under any name")
	}
	assert.Contains(t, cap.env, tokenEnv+"=mapped-token")
}

func TestMirrorPinsLayoutAgainstHostConfig(t *testing.T) {
	// The layout <workspace>/<owner>/<repo> is what goldfinger prints, reports, and
	// reconciles against, so every ghorg knob that could move clones must be both
	// pinned in argv (a CLI flag overrides env AND config) and scrubbed from the
	// child env. Setting all of them here must not change the resulting layout.
	t.Setenv("GHORG_OUTPUT_DIR", "host-output")
	t.Setenv("GHORG_PRESERVE_SCM_HOSTNAME", "true")
	t.Setenv("GHORG_PRESERVE_DIRECTORY_STRUCTURE", "true")

	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "tok", Options{}))

	assert.Contains(t, cap.args, "--output-dir=redscaresu",
		"output dir must be pinned to the owner so GHORG_OUTPUT_DIR can't relocate clones")
	assert.Contains(t, cap.args, "--preserve-scm-hostname=false",
		"scm-hostname nesting must be pinned off so clones stay directly under <ws>/<owner>")
	for _, e := range cap.env {
		for _, banned := range layoutGhorgEnv {
			assert.NotContains(t, e, banned+"=", "layout-changing ghorg var %s must be stripped", banned)
		}
	}
}

func TestMirrorOrgCloneType(t *testing.T) {
	s := userSelection()
	s.OwnerType = models.OwnerOrganization
	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, s, "tok", Options{}))
	assert.Contains(t, cap.args, "--clone-type=org")
}

func TestMirrorDryRun(t *testing.T) {
	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "tok", Options{DryRun: true}))
	assert.Contains(t, cap.args, "--dry-run")
}

func TestMirrorNoClean(t *testing.T) {
	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "tok", Options{}))
	assert.NotContains(t, cap.args, "--no-clean", "omitted by default")

	cap = capture{}
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "tok", Options{NoClean: true}))
	assert.Contains(t, cap.args, "--no-clean")
}

func TestMirrorBranch(t *testing.T) {
	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "tok", Options{}))
	for _, a := range cap.args {
		assert.NotContains(t, a, "--branch", "omitted by default so ghorg uses each repo's default")
	}

	cap = capture{}
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "tok", Options{Branch: "dev"}))
	assert.Contains(t, cap.args, "--branch=dev")
}

func TestMirrorEmptySelection(t *testing.T) {
	err := Mirror(context.Background(), func(context.Context, string, []string, []string) error {
		t.Fatal("runner should not be called for an empty selection")
		return nil
	}, models.Selection{Owner: "x"}, "tok", Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

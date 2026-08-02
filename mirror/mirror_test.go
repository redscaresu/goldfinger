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

func TestMirrorEmptySelection(t *testing.T) {
	err := Mirror(context.Background(), func(context.Context, string, []string, []string) error {
		t.Fatal("runner should not be called for an empty selection")
		return nil
	}, models.Selection{Owner: "x"}, "tok", Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

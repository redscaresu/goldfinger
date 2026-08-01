package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGuideRenders(t *testing.T) {
	// guide needs no token and no network.
	out, err := executeCmd(t, "", "guide")
	require.NoError(t, err)
	for _, want := range []string{"select", "mirror", "apply", "dry-run", "GOLD_FINGER_PAT"} {
		assert.Contains(t, out, want)
	}
}

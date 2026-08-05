package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestExitErrorMessage locks the other half of the exit-code contract (the code
// mapping itself is covered by TestExitCode): a code-only exitError — as `check`
// uses for drift — prints nothing, because its report already went to stdout,
// while an exitError wrapping a real error surfaces that message on stderr.
func TestExitErrorMessage(t *testing.T) {
	assert.Empty(t, exitError{code: 1}.Error())
	assert.Equal(t, "bad flag", exitError{code: 2, err: errors.New("bad flag")}.Error())
}

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEmitJSONFormat locks emitJSON's one contract that varies: compact=true
// (machine mode / --quiet) emits single-line JSON, compact=false emits the
// indented human form, and both carry identical content — only whitespace
// differs, so the schema golden that pins the indented rendering is untouched.
func TestEmitJSONFormat(t *testing.T) {
	v := map[string]any{"b": 2, "a": map[string]any{"c": "x"}}

	var pretty, compact bytes.Buffer
	require.NoError(t, emitJSON(&pretty, v, false))
	require.NoError(t, emitJSON(&compact, v, true))

	// Both terminate with exactly one trailing newline (the stdout=data contract
	// expects a clean line, not a bare blob).
	assert.True(t, strings.HasSuffix(pretty.String(), "}\n"))
	assert.True(t, strings.HasSuffix(compact.String(), "}\n"))

	// Compact is one line; pretty is indented and multi-line.
	assert.Equal(t, 1, strings.Count(compact.String(), "\n"), "compact JSON is one line + trailing newline")
	assert.NotContains(t, compact.String(), "\n  ")
	assert.Contains(t, pretty.String(), "\n  ", "pretty JSON is indented")
	assert.Less(t, compact.Len(), pretty.Len())

	// Identical content regardless of format.
	var fromPretty, fromCompact map[string]any
	require.NoError(t, json.Unmarshal(pretty.Bytes(), &fromPretty))
	require.NoError(t, json.Unmarshal(compact.Bytes(), &fromCompact))
	assert.Equal(t, fromPretty, fromCompact)
}

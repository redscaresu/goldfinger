package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStylerOffForNonTTY(t *testing.T) {
	var buf bytes.Buffer
	s := newStyler(&buf)
	assert.False(t, s.on, "a bytes.Buffer is not a TTY, so styling is off")
	assert.Equal(t, "plain", s.paint(cGreen, "plain"))
}

func TestPaintWrapsWhenOn(t *testing.T) {
	s := styler{on: true}
	assert.Equal(t, cGreen+"hi"+cReset, s.paint(cGreen, "hi"))
}

func TestWriterIsTTY(t *testing.T) {
	assert.False(t, writerIsTTY(&bytes.Buffer{}), "a buffer is not a TTY")

	// A regular file is an *os.File but not a character device.
	f, err := os.Create(filepath.Join(t.TempDir(), "f"))
	require.NoError(t, err)
	defer f.Close()
	assert.False(t, writerIsTTY(f), "a regular file is not a TTY")
}

func TestBannerDoneWriteWithoutAnsiOnNonTTY(t *testing.T) {
	var buf bytes.Buffer
	banner(&buf, "Phase title")
	done(&buf, "finished the thing")

	out := buf.String()
	assert.Contains(t, out, "Phase title")
	assert.Contains(t, out, "finished the thing")
	assert.NotContains(t, out, "\033[", "no ANSI escape codes when not a TTY")
}

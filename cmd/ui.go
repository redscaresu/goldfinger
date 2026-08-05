package main

import (
	"fmt"
	"io"
	"os"
)

// ANSI styles, emitted only when writing to a TTY with NO_COLOR unset.
const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cGreen  = "\033[32m"
	cCyan   = "\033[36m"
	cRed    = "\033[31m"
	cYellow = "\033[33m"
)

// writerIsTTY reports whether w is a terminal. In tests the command output is a
// bytes.Buffer (not an *os.File), so styling is off and assertions stay clean.
func writerIsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

type styler struct{ on bool }

func newStyler(w io.Writer) styler {
	return styler{on: os.Getenv("NO_COLOR") == "" && writerIsTTY(w)}
}

func (s styler) paint(code, text string) string {
	if !s.on {
		return text
	}
	return code + text + cReset
}

// banner prints a phase header (to stderr — stdout stays reserved for data).
func banner(w io.Writer, title string) {
	s := newStyler(w)
	fmt.Fprintf(w, "\n%s %s\n", s.paint(cBold+cCyan, "▶"), s.paint(cBold, title))
}

// done prints a success summary line.
func done(w io.Writer, msg string) {
	s := newStyler(w)
	fmt.Fprintf(w, "%s %s\n", s.paint(cGreen, "✓"), msg)
}

// warn prints a caution line (to stderr — stdout stays reserved for data). It is
// for outcomes worth the operator's attention that are not hard failures, e.g. a
// mirror that under-covered its selection.
func warn(w io.Writer, msg string) {
	s := newStyler(w)
	fmt.Fprintf(w, "%s %s\n", s.paint(cYellow, "⚠"), msg)
}

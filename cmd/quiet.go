package main

import (
	"io"

	"github.com/spf13/cobra"
)

const quietFlagName = "quiet"

func quietRequested(cmd *cobra.Command) bool {
	quiet, err := cmd.Flags().GetBool(quietFlagName)
	return err == nil && quiet
}

func quietWriter(w io.Writer, quiet bool) io.Writer {
	if quiet {
		return io.Discard
	}
	return w
}

func humanErr(cmd *cobra.Command) io.Writer {
	return quietWriter(cmd.ErrOrStderr(), quietRequested(cmd))
}

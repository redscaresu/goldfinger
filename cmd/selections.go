package main

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/redscaresu/goldfinger/selection"
	"github.com/spf13/cobra"
)

// selectionsReport is the --json shape for `selections`: a versioned wrapper
// (not a bare array, so it carries `version` per issue #27 §4). An unreadable
// entry is represented inline with an `error` field rather than dropped, and an
// empty registry emits an empty `selections: []`, not an error.
type selectionsReport struct {
	Version    int                  `json:"version"`
	Selections []selectionEntryJSON `json:"selections"`
}

// selectionEntryJSON is one registry entry. For a readable entry, error is empty
// and repoCount is always present (0 for a valid --allow-empty selection); for an
// unreadable one, owner/resolvedAt are empty, repoCount is null, and error carries
// the reason. repoCount is a pointer precisely so "readable, zero repos" (0) is
// distinguishable from "unreadable" (null).
type selectionEntryJSON struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Owner      string `json:"owner,omitempty"`
	RepoCount  *int   `json:"repoCount"`
	ResolvedAt string `json:"resolvedAt,omitempty"`
	Error      string `json:"error,omitempty"`
}

type selectionsOptions struct {
	asJSON bool
	quiet  bool
}

func newSelectionsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "selections",
		Short: "List named selections in the registry",
		RunE: func(cmd *cobra.Command, args []string) error {
			names, err := selection.Names()
			if err != nil {
				return err
			}
			return runSelections(names, selectionsOptions{asJSON: asJSON, quiet: quietRequested(cmd)}, cmd.OutOrStdout(), humanErr(cmd))
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"emit the registry as a versioned JSON wrapper on stdout (unreadable entries carry an `error` field; an empty registry is an empty list, not an error)")
	return cmd
}

func runSelections(names []string, opts selectionsOptions, out, errOut io.Writer) error {
	errOut = quietWriter(errOut, opts.quiet)
	if opts.asJSON {
		return emitSelectionsJSON(out, names, opts.quiet)
	}
	if opts.quiet {
		return nil
	}
	return renderSelectionsTable(out, errOut, names)
}

// renderSelectionsTable writes the human table. An empty registry prints a
// create-one hint to stderr (keeping stdout empty), and an unreadable entry shows
// "(unreadable)" rather than aborting the listing.
func renderSelectionsTable(out, errOut io.Writer, names []string) error {
	if len(names) == 0 {
		fmt.Fprintln(errOut, "no named selections yet — create one with: goldfinger select --name <name> ...")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tOWNER\tREPOS\tRESOLVED")
	for _, n := range names {
		path, err := selection.PathForName(n)
		if err != nil {
			return err
		}
		sel, err := selection.Read(path)
		if err != nil {
			fmt.Fprintf(tw, "%s\t(unreadable)\t\t\n", n)
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", n, sel.Owner, len(sel.Repos), sel.ResolvedAt.Format("2006-01-02"))
	}
	return tw.Flush()
}

// emitSelectionsJSON builds and writes the versioned registry payload. An
// unreadable entry is kept with an `error` field (mirroring the tolerant table),
// and PathForName failures surface as that entry's error too rather than aborting
// the whole listing — an agent still gets every name.
func emitSelectionsJSON(out io.Writer, names []string, quiet bool) error {
	rep := selectionsReport{
		Version:    selectionsReportVersion,
		Selections: make([]selectionEntryJSON, 0, len(names)),
	}
	for _, n := range names {
		entry := selectionEntryJSON{Name: n}
		path, err := selection.PathForName(n)
		if err != nil {
			entry.Error = err.Error()
			rep.Selections = append(rep.Selections, entry)
			continue
		}
		entry.Path = path
		sel, err := selection.Read(path)
		if err != nil {
			entry.Error = err.Error()
			rep.Selections = append(rep.Selections, entry)
			continue
		}
		entry.Owner = sel.Owner
		n := len(sel.Repos)
		entry.RepoCount = &n
		entry.ResolvedAt = sel.ResolvedAt.Format(time.RFC3339)
		rep.Selections = append(rep.Selections, entry)
	}
	return emitJSON(out, rep, quiet)
}

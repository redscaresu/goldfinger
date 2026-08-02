package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/redscaresu/goldfinger/selection"
	"github.com/spf13/cobra"
)

func newSelectionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "selections",
		Short: "List named selections in the registry",
		RunE: func(cmd *cobra.Command, args []string) error {
			names, err := selection.Names()
			if err != nil {
				return err
			}
			if len(names) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no named selections yet — create one with: goldfinger select --name <name> ...")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
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
		},
	}
}

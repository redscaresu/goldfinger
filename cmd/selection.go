package main

import (
	"fmt"
	"strings"

	"github.com/redscaresu/goldfinger/selection"
	"github.com/spf13/cobra"
)

// addSelectionFlags binds the --name / --selection flags shared by select,
// mirror, apply, and check.
func addSelectionFlags(cmd *cobra.Command, name, path *string) {
	f := cmd.Flags()
	f.StringVar(name, "name", "", "named selection in the registry (~/.config/goldfinger/selections)")
	f.StringVar(path, "selection", "", "explicit path to the selection lockfile (default ./goldfinger.selection)")
}

// resolveSelectionPath turns the --name / --selection flags into the lockfile
// path. Exactly one may be set; a name maps into the registry, a path is used
// verbatim, and if neither is given the default file in the CWD is used.
func resolveSelectionPath(name, path string) (string, error) {
	if name != "" && path != "" {
		return "", fmt.Errorf("--name and --selection are mutually exclusive")
	}
	if name != "" {
		if err := validSelectionName(name); err != nil {
			return "", err
		}
		return selection.PathForName(name)
	}
	if path != "" {
		return path, nil
	}
	return defaultSelectionPath, nil
}

// validSelectionName rejects names that are not safe single-segment filenames,
// so a name can never escape the registry directory.
func validSelectionName(name string) error {
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." || strings.Contains(name, "..") {
		return fmt.Errorf("invalid selection name %q: use a simple name without path separators or ..", name)
	}
	return nil
}

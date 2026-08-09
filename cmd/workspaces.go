package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// workspaceManifestName is the sidecar file `mirror --purpose` drops at the root
// of each snapshot workspace. It records the snapshot's identity as separate
// fields so `workspaces list`/`prune` can filter reliably instead of parsing the
// ambiguous directory name (a purpose and a sanitised branch can both contain
// '-', so the name alone can't be split back into its parts — issue #29).
const workspaceManifestName = "goldfinger-workspace.json"

// stampLayout is the timestamp format resolveWorkspace stamps into a --purpose
// snapshot's directory name, and the format `workspaces` parses back out of a
// manifest-less directory to recover its creation time.
const stampLayout = "2006-01-02-150405.000"

// workspaces actions (the single positional argument).
const (
	workspaceActionList  = "list"
	workspaceActionPrune = "prune"
)

// ageSugarRE matches a whole-number day ("7d") or week ("2w") age — the sugar
// parseAgeDuration adds on top of Go's duration syntax. An optional leading '-'
// is matched so a negative like "-7d" parses to a negative duration and is
// rejected by runWorkspaces' clear "must not be negative" check, rather than
// falling through to time.ParseDuration's opaque "unknown unit d" error.
var ageSugarRE = regexp.MustCompile(`^(-?\d+)([dw])$`)

// parseAgeDuration parses a snapshot age. It accepts everything Go's
// time.ParseDuration does (e.g. "36h", "90m", "1h30m") and additionally the day
// ("d") and week ("w") units Go lacks — "7d" == 168h, "2w" == 336h — because
// operators reach for --older-than in days and weeks, not hours. The d/w sugar is
// deliberately a single whole-number-plus-unit form (no "1w3d" compounding) so it
// stays unambiguous; anything else is handed to time.ParseDuration unchanged.
func parseAgeDuration(s string) (time.Duration, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, errors.New("empty duration")
	}
	if m := ageSugarRE.FindStringSubmatch(trimmed); m != nil {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			// Only an out-of-range integer reaches here (the regex already
			// guaranteed digits); surface it plainly rather than as a parse of "d"/"w".
			return 0, fmt.Errorf("invalid duration %q: %w", s, err)
		}
		unit := 24 * time.Hour
		if m[2] == "w" {
			unit = 7 * 24 * time.Hour
		}
		return time.Duration(n) * unit, nil
	}
	return time.ParseDuration(trimmed)
}

// ageDuration is the pflag.Value backing --older-than. It stores a time.Duration
// but parses via parseAgeDuration, so the flag accepts "7d"/"2w" alongside Go
// durations while runWorkspaces keeps consuming a plain time.Duration.
type ageDuration struct{ d time.Duration }

func (a *ageDuration) String() string { return a.d.String() }

func (a *ageDuration) Set(s string) error {
	d, err := parseAgeDuration(s)
	if err != nil {
		return err
	}
	a.d = d
	return nil
}

// Type is the value-kind cobra shows in usage; "duration" keeps the help reading
// like the Go-duration flag it still fundamentally is.
func (a *ageDuration) Type() string { return "duration" }

// interface check: ageDuration must satisfy pflag.Value to back a flag.
var _ pflag.Value = (*ageDuration)(nil)

// stampSuffixRE matches the trailing "-<stamp>" that resolveWorkspace appends to
// every --purpose workspace, e.g. "audit-dev-2026-08-05-101112.131". It is the
// sole recogniser of a goldfinger snapshot directory: `list`/`prune` act only on
// directories whose name ends this way, so they can never enumerate — let alone
// delete — a directory goldfinger did not stamp (the default ~/goldfinger/<owner>
// mirror, or any unrelated dir). The pattern is specific enough that a GitHub
// owner login can't collide with it.
var stampSuffixRE = regexp.MustCompile(`-(\d{4}-\d{2}-\d{2}-\d{6}\.\d{3})$`)

// workspaceManifest is the sidecar payload written into each --purpose snapshot.
// purpose/branch/stamp/owner are recorded as separate fields precisely because
// the directory name cannot be reliably split back into them.
type workspaceManifest struct {
	Version   int       `json:"version"`
	Purpose   string    `json:"purpose"`
	Branch    string    `json:"branch,omitempty"`
	Stamp     string    `json:"stamp"`
	Owner     string    `json:"owner"`
	CreatedAt time.Time `json:"createdAt"`
}

// workspacesReport is the machine-readable payload `workspaces list`/`prune`
// emit with --json. list reports every snapshot; prune reports the subset it
// matched (and would remove, or removed when pruned is true).
type workspacesReport struct {
	Version    int             `json:"version"`
	Root       string          `json:"root"`
	Action     string          `json:"action"` // "list" | "prune"
	Pruned     bool            `json:"pruned"` // prune only: true once --confirm actually deleted the matches
	Workspaces []workspaceInfo `json:"workspaces"`
}

// workspaceInfo is one snapshot in the report. purpose/branch/owner/createdAt are
// authoritative only when manifestPresent is true; for a manifest-less legacy
// snapshot they are empty (createdAt is still recovered from the dir-name stamp),
// so a consumer must not treat the raw directory name as a reliable split.
type workspaceInfo struct {
	Path            string `json:"path"`
	Purpose         string `json:"purpose,omitempty"`
	Branch          string `json:"branch,omitempty"`
	Stamp           string `json:"stamp,omitempty"`
	Owner           string `json:"owner,omitempty"`
	SizeBytes       int64  `json:"sizeBytes"`
	CreatedAt       string `json:"createdAt,omitempty"` // RFC 3339
	ManifestPresent bool   `json:"manifestPresent"`
}

// workspacesOptions bundles the command's resolved inputs so runWorkspaces stays
// within the argument budget.
type workspacesOptions struct {
	action    string
	root      string
	olderThan time.Duration
	purpose   string
	confirm   bool
	asJSON    bool
	quiet     bool
}

func newWorkspacesCmd() *cobra.Command {
	var (
		root      string
		olderThan ageDuration
		purpose   string
		confirm   bool
		asJSON    bool
	)
	cmd := &cobra.Command{
		Use:   "workspaces (list | prune)",
		Short: "List or prune ephemeral mirror snapshot workspaces",
		Long: "workspaces manages the timestamped snapshot directories `mirror --purpose` " +
			"creates under the workspace root (default ~/goldfinger). goldfinger never " +
			"deletes a snapshot on its own — this is the safe, first-class way to see and " +
			"reclaim old ones.\n\n" +
			"  goldfinger workspaces list   — enumerate snapshots with size and creation time.\n" +
			"  goldfinger workspaces prune  — remove snapshots, but only after showing what it " +
			"would delete: prune previews by default and deletes only with --confirm (the same " +
			"posture as apply). Narrow the target with --older-than <dur> and/or --purpose <name>.\n\n" +
			"It is filesystem-only: it never touches GitHub and runs no git. Deletion is the one " +
			"mutating action and it is gated behind --confirm.",
		ValidArgs: []string{workspaceActionList, workspaceActionPrune},
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaces(workspacesOptions{
				action:    args[0],
				root:      root,
				olderThan: olderThan.d,
				purpose:   purpose,
				confirm:   confirm,
				asJSON:    asJSON,
				quiet:     quietRequested(cmd),
			}, cmd.OutOrStdout(), humanErr(cmd))
		},
	}
	f := cmd.Flags()
	f.StringVar(&root, "root", "", "workspace root to scan (default ~/goldfinger)")
	f.Var(&olderThan, "older-than", "prune: only snapshots older than this age, e.g. 7d, 2w, or a Go duration like 168h (0 = no age filter)")
	f.StringVar(&purpose, "purpose", "", "prune: only snapshots whose manifest records exactly this purpose (manifest-less snapshots are never matched)")
	f.BoolVar(&confirm, "confirm", false, "prune: actually delete the matched snapshots (without it, prune only previews)")
	f.BoolVar(&asJSON, "json", false, "emit the workspace listing as JSON on stdout (stderr keeps the human banners)")
	return cmd
}

// runWorkspaces is the testable core: it resolves the root, scans it for snapshot
// directories, then either lists or prunes. It runs no git and makes no network
// call — the only mutating action is a confirm-gated delete under prune.
func runWorkspaces(opts workspacesOptions, out, errOut io.Writer) error {
	errOut = quietWriter(errOut, opts.quiet)
	if opts.action == workspaceActionList && (opts.olderThan != 0 || opts.purpose != "" || opts.confirm) {
		return errors.New("--older-than, --purpose, and --confirm apply only to `workspaces prune`, not `list`")
	}
	// A negative age is not "no filter": treating it as 0 would silently match
	// every snapshot, so `prune --older-than=-1h --confirm` would delete the lot.
	// Reject it before we ever scan or delete.
	if opts.olderThan < 0 {
		return errors.New("--older-than must not be negative")
	}

	root, err := resolveWorkspaceRoot(opts.root)
	if err != nil {
		return err
	}
	all, err := scanWorkspaces(root)
	if err != nil {
		return err
	}

	switch opts.action {
	case workspaceActionList:
		if opts.asJSON {
			return emitJSON(out, workspacesReport{
				Version: workspacesReportVersion, Root: root,
				Action: workspaceActionList, Pruned: false, Workspaces: nonNilWorkspaces(all),
			}, opts.quiet)
		}
		if len(all) == 0 {
			banner(errOut, "no snapshot workspaces under "+root)
			return nil
		}
		if opts.quiet {
			return nil
		}
		printWorkspaceList(out, all)
		return nil
	case workspaceActionPrune:
		return runPrune(opts, root, all, out, errOut)
	default:
		// Unreachable: cobra's OnlyValidArgs already rejects any other action.
		return fmt.Errorf("unknown workspaces action %q (use %q or %q)", opts.action, workspaceActionList, workspaceActionPrune)
	}
}

// runPrune selects the snapshots matching the age/purpose filters, then previews
// them (default) or deletes them (--confirm). Nothing is ever removed without
// --confirm — the whole point of the command's safety posture.
func runPrune(opts workspacesOptions, root string, all []workspaceInfo, out, errOut io.Writer) error {
	matched := filterForPrune(all, opts.olderThan, opts.purpose, nowFunc())
	pruned := false

	switch {
	case len(matched) == 0:
		banner(errOut, "no snapshots match — nothing to prune")
	case opts.confirm:
		if err := deleteWorkspaces(root, matched, errOut); err != nil {
			return err
		}
		done(errOut, fmt.Sprintf("pruned %d snapshot(s), reclaimed %s", len(matched), humanSize(totalSize(matched))))
		pruned = true
	default:
		banner(errOut, fmt.Sprintf("would remove %d snapshot(s), reclaiming %s — pass --confirm to delete:", len(matched), humanSize(totalSize(matched))))
		for _, w := range matched {
			fmt.Fprintf(errOut, "  %s  (%s)\n", w.Path, humanSize(w.SizeBytes))
		}
	}

	if opts.asJSON {
		return emitJSON(out, workspacesReport{
			Version: workspacesReportVersion, Root: root,
			Action: workspaceActionPrune, Pruned: pruned, Workspaces: nonNilWorkspaces(matched),
		}, opts.quiet)
	}
	return nil
}

// deleteWorkspaces removes each matched snapshot, re-checking safeToRemove
// immediately before the destructive call as defence in depth: a snapshot must be
// a direct, stamp-suffixed child of the resolved root, so a bug upstream can never
// turn this into an arbitrary recursive delete.
func deleteWorkspaces(root string, matched []workspaceInfo, errOut io.Writer) error {
	for _, w := range matched {
		if !safeToRemove(root, w.Path) {
			return fmt.Errorf("refusing to remove %q: not a snapshot directory directly under %q", w.Path, root)
		}
		// Re-stat immediately before the destructive call: the candidate must
		// still be a real directory, not a symlink someone slipped in after the
		// scan (os.Lstat does not follow the link, so a symlink fails IsDir).
		if fi, err := os.Lstat(w.Path); err != nil || !fi.IsDir() {
			return fmt.Errorf("refusing to remove %q: not a directory", w.Path)
		}
		if err := os.RemoveAll(w.Path); err != nil {
			return fmt.Errorf("remove %s: %w", w.Path, err)
		}
		done(errOut, "removed "+w.Path)
	}
	return nil
}

// safeToRemove reports whether path is a direct child of root whose name carries
// the snapshot stamp suffix — the only shape prune is ever allowed to delete.
func safeToRemove(root, path string) bool {
	return filepath.Dir(path) == root && stampSuffixRE.MatchString(filepath.Base(path))
}

// resolveWorkspaceRoot returns the absolute directory to scan: --root if given,
// else ~/goldfinger (the default mirror workspace root). It canonicalises the
// path through any symlinks so the safeToRemove parent-dir check compares real
// paths — a symlinked root must not let a delete escape it. A not-yet-created
// root (nobody has mirrored) has nothing to scan or delete, so its literal
// absolute path is returned unchanged.
func resolveWorkspaceRoot(root string) (string, error) {
	var base string
	if root != "" {
		abs, err := filepath.Abs(root)
		if err != nil {
			return "", fmt.Errorf("resolve workspace root: %w", err)
		}
		base = abs
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir for workspace root: %w", err)
		}
		base = filepath.Join(home, "goldfinger")
	}
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		return resolved, nil
	}
	return base, nil
}

// scanWorkspaces enumerates the snapshot directories directly under root, sorted
// by path. A non-existent root (nobody has mirrored yet) is an empty result, not
// an error.
func scanWorkspaces(root string) ([]workspaceInfo, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read workspace root %s: %w", root, err)
	}
	var out []workspaceInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		stamp, ok := snapshotStamp(e.Name())
		if !ok {
			continue
		}
		info, err := describeWorkspace(filepath.Join(root, e.Name()), stamp)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// snapshotStamp returns the trailing timestamp of a snapshot directory name and
// whether the name is a snapshot at all.
func snapshotStamp(name string) (string, bool) {
	m := stampSuffixRE.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// describeWorkspace builds one snapshot's info: its on-disk size, a creation time
// (from the manifest if present, else parsed from the dir-name stamp), and the
// manifest's structured purpose/branch/owner when the sidecar is present.
func describeWorkspace(path, stamp string) (workspaceInfo, error) {
	size, err := dirSize(path)
	if err != nil {
		return workspaceInfo{}, err
	}
	info := workspaceInfo{Path: path, Stamp: stamp, SizeBytes: size}
	if t, err := time.ParseInLocation(stampLayout, stamp, time.Local); err == nil {
		info.CreatedAt = t.Format(time.RFC3339)
	}
	if m, ok := readWorkspaceManifest(path); ok {
		info.ManifestPresent = true
		info.Purpose = m.Purpose
		info.Branch = m.Branch
		info.Owner = m.Owner
		if m.Stamp != "" {
			info.Stamp = m.Stamp
		}
		if !m.CreatedAt.IsZero() {
			info.CreatedAt = m.CreatedAt.Format(time.RFC3339)
		}
	}
	return info, nil
}

// readWorkspaceManifest reads and parses a snapshot's sidecar manifest. A missing
// or unparseable manifest is reported as absent (ok=false), never an error: a
// legacy snapshot created before manifests, or a hand-made directory, is still a
// valid thing to list and prune — just without structured metadata.
func readWorkspaceManifest(dir string) (workspaceManifest, bool) {
	data, err := os.ReadFile(filepath.Join(dir, workspaceManifestName)) //nolint:gosec // G304: dir is a mirror-workspace directory goldfinger is listing/pruning; reading its own manifest from a path it just walked is the intended behaviour.
	if err != nil {
		return workspaceManifest{}, false
	}
	var m workspaceManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return workspaceManifest{}, false
	}
	return m, true
}

// dirSize sums the sizes of the regular files under path.
func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		total += fi.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure %s: %w", path, err)
	}
	return total, nil
}

// filterForPrune narrows the snapshots to those the prune filters select. With no
// filter every snapshot matches (a full clear, still gated by --confirm). Both
// filters are conservative: --purpose matches only manifest-tagged snapshots
// whose recorded purpose is exactly name (a manifest-less snapshot is never
// matched by purpose), and --older-than skips any snapshot whose age can't be
// determined — so an ambiguous snapshot is kept, not deleted.
func filterForPrune(all []workspaceInfo, olderThan time.Duration, purpose string, now time.Time) []workspaceInfo {
	var out []workspaceInfo
	for _, w := range all {
		if purpose != "" && (!w.ManifestPresent || w.Purpose != purpose) {
			continue
		}
		if olderThan > 0 {
			created, ok := workspaceCreatedAt(w)
			if !ok || now.Sub(created) < olderThan {
				continue
			}
		}
		out = append(out, w)
	}
	return out
}

// workspaceCreatedAt parses a snapshot's recorded creation time (RFC 3339). ok is
// false when it is absent or unparseable.
func workspaceCreatedAt(w workspaceInfo) (time.Time, bool) {
	if w.CreatedAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, w.CreatedAt)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// totalSize sums the on-disk size of a set of snapshots.
func totalSize(ws []workspaceInfo) int64 {
	var total int64
	for _, w := range ws {
		total += w.SizeBytes
	}
	return total
}

// printWorkspaceList writes the human listing to stdout (the primary output of
// `list`): one tab-separated row per snapshot, plus a reclaimable-total footer.
func printWorkspaceList(out io.Writer, ws []workspaceInfo) {
	for _, w := range ws {
		created := w.CreatedAt
		if created == "" {
			created = "unknown"
		}
		fmt.Fprintf(out, "%s\t%s\t%s\n", w.Path, humanSize(w.SizeBytes), created)
	}
	fmt.Fprintf(out, "%d snapshot(s), %s total\n", len(ws), humanSize(totalSize(ws)))
}

// nonNilWorkspaces returns ws, or an empty (non-nil) slice, so the JSON report's
// "workspaces" is always [] rather than null.
func nonNilWorkspaces(ws []workspaceInfo) []workspaceInfo {
	if ws == nil {
		return []workspaceInfo{}
	}
	return ws
}

// humanSize renders a byte count in IEC units to one decimal place.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// writeWorkspaceManifest persists a snapshot's sidecar manifest at the workspace
// root. It is called after a successful, non-dry-run `mirror --purpose`, so
// `workspaces` gets reliable structured metadata for that snapshot.
func writeWorkspaceManifest(ws string, m workspaceManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("render workspace manifest: %w", err)
	}
	path := filepath.Join(ws, workspaceManifestName)
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write workspace manifest: %w", err)
	}
	// WriteFile only applies the mode on creation, so rewriting a manifest left
	// 0644 by an older goldfinger would keep the looser mode; chmod makes 0600
	// hold on rewrite too.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure workspace manifest perms: %w", err)
	}
	return nil
}

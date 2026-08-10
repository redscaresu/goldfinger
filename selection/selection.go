// Package selection reads and writes the goldfinger selection lockfile — the
// frozen, reviewable set of repos that both `mirror` and `apply` consume.
package selection

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/redscaresu/goldfinger/models"
)

// nameExt is the file extension for named selections in the registry.
const nameExt = ".json"

// WriteOptions tunes how Write publishes the lockfile.
type WriteOptions struct {
	// Overwrite replaces an existing lockfile at path (the refresh semantics of
	// `select`). When false, Write refuses to clobber an existing file: it
	// publishes with an atomic create-or-fail link, so two concurrent writers to
	// the same path yield exactly one success and the loser gets an error that
	// unwraps to fs.ErrExist. MCP handlers run concurrently, so this is the
	// difference between a race and a guarantee.
	Overwrite bool
}

// Write serialises s to path as indented JSON, published atomically so a reader
// never sees a half-written lockfile. It writes to a uniquely-named temp file in
// the *target directory* (a same-filesystem rename/link is an atomic metadata
// op; a cross-device one would fail), fsyncs the file bytes and the parent
// directory so the entry survives a crash, then either renames over any existing
// file (opts.Overwrite) or hard-links create-or-fail (see WriteOptions). The
// parent directory is created if needed. The temp file never lingers: every
// return path removes it (a successful rename consumes it).
func Write(path string, s models.Selection, opts WriteOptions) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal selection: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if dir != "." {
		// 0750 applies only when MkdirAll creates the dir; a pre-existing
		// selections dir keeps its mode. We deliberately don't chmod it back —
		// the operator owns that directory and its metadata isn't secret; the
		// lockfile itself is written 0600 (os.CreateTemp's mode) below.
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create selection dir: %w", err)
		}
	}

	// A random suffix means concurrent writers never collide on the temp path
	// itself — only on the final rename/link, which is where we want the race
	// resolved. os.CreateTemp creates the file 0600.
	tmp, err := os.CreateTemp(dir, ".selection-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp selection: %w", err)
	}
	tmpName := tmp.Name()

	if err := writeAndSync(tmp, data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close selection: %w", err)
	}

	if opts.Overwrite {
		if err := os.Rename(tmpName, path); err != nil {
			_ = os.Remove(tmpName)
			return fmt.Errorf("finalise selection: %w", err)
		}
	} else {
		// Link fails with EEXIST if path already exists — a race-free
		// create-or-fail. The wrapped error still unwraps to fs.ErrExist.
		if err := os.Link(tmpName, path); err != nil {
			_ = os.Remove(tmpName)
			return fmt.Errorf("create selection %s: %w", path, err)
		}
		_ = os.Remove(tmpName)
	}

	syncDir(dir)
	return nil
}

// writeAndSync fills the temp file and fsyncs its bytes so they survive a crash
// before the rename/link below publishes the directory entry.
func writeAndSync(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write selection: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync selection: %w", err)
	}
	return nil
}

// syncDir fsyncs a directory so a freshly-created or renamed entry within it
// survives a crash. Best-effort: some filesystems reject opening a directory for
// sync, and the file's own bytes are already fsynced, so a failure here is not
// fatal to the write's durability contract.
func syncDir(dir string) {
	d, err := os.Open(dir) //nolint:gosec // G304: dir is the selection's own parent directory, resolved and created above.
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// Read loads and validates the selection lockfile at path.
func Read(path string) (models.Selection, error) {
	sel, _, err := ReadWithDigest(path)
	return sel, err
}

// ReadWithDigest loads and validates the lockfile at path and also returns the
// SelectionBytesDigest of the exact bytes it read — computed over the SAME
// buffer it parses, so the digest provably identifies the content that produced
// the returned selection. There is no re-read a concurrent writer could slip
// between, which is what makes `apply --expect-selection-sha256` a real binding
// and not a best-effort guess.
func ReadWithDigest(path string) (models.Selection, string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the selection lockfile location goldfinger resolves (a named selection under its config dir or an explicit --selection path); reading the operator's own lockfile is the point.
	if err != nil {
		if os.IsNotExist(err) {
			return models.Selection{}, "", fmt.Errorf("no selection at %s — run `goldfinger select` first", path)
		}
		return models.Selection{}, "", fmt.Errorf("read selection: %w", err)
	}
	sel, err := parseSelection(data, path)
	if err != nil {
		return models.Selection{}, "", err
	}
	return sel, SelectionBytesDigest(data), nil
}

// parseSelection unmarshals and version-validates lockfile bytes. It is shared
// by Read and ReadWithDigest so the accepted-version rule lives in one place.
func parseSelection(data []byte, path string) (models.Selection, error) {
	var s models.Selection
	if err := json.Unmarshal(data, &s); err != nil {
		return models.Selection{}, fmt.Errorf("parse selection %s: %w", path, err)
	}
	// Accept every schema version this goldfinger can read. A v1 lockfile has no
	// branch-presence metadata; it migrates in memory to empty branch facts,
	// which read back as "unknown" (RecordedBranch) — never guessed.
	switch s.Version {
	case 1, models.SelectionVersion:
	default:
		return models.Selection{}, fmt.Errorf("selection %s has unsupported version %d (this goldfinger understands versions 1..%d)", path, s.Version, models.SelectionVersion)
	}
	return s, nil
}

// Dir is the directory holding named selections. It honours XDG_CONFIG_HOME,
// falling back to ~/.config/goldfinger/selections (matching ghorg's convention).
func Dir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "goldfinger", "selections"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", "goldfinger", "selections"), nil
}

// PathForName maps a named selection to its lockfile path under Dir().
func PathForName(name string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+nameExt), nil
}

// Names lists the stored named selections, sorted. A missing registry directory
// is not an error — it just means none exist yet.
func Names() ([]string, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read selections dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), nameExt) {
			names = append(names, strings.TrimSuffix(e.Name(), nameExt))
		}
	}
	sort.Strings(names)
	return names, nil
}

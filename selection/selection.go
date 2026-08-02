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

// Write serialises s to path as indented JSON. The write is atomic: it writes a
// sibling temp file and renames it into place, so a reader never sees a
// half-written lockfile. The parent directory is created if needed.
func Write(path string, s models.Selection) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal selection: %w", err)
	}
	data = append(data, '\n')

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create selection dir: %w", err)
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write selection: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("finalise selection: %w", err)
	}
	return nil
}

// Read loads and validates the selection lockfile at path.
func Read(path string) (models.Selection, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return models.Selection{}, fmt.Errorf("no selection at %s — run `goldfinger select` first", path)
		}
		return models.Selection{}, fmt.Errorf("read selection: %w", err)
	}
	var s models.Selection
	if err := json.Unmarshal(data, &s); err != nil {
		return models.Selection{}, fmt.Errorf("parse selection %s: %w", path, err)
	}
	if s.Version != models.SelectionVersion {
		return models.Selection{}, fmt.Errorf("selection %s has unsupported version %d (this goldfinger understands version %d)", path, s.Version, models.SelectionVersion)
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

// Package selection reads and writes the goldfinger selection lockfile — the
// frozen, reviewable set of repos that both `mirror` and `apply` consume.
package selection

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/redscaresu/goldfinger/models"
)

// Write serialises s to path as indented JSON. The write is atomic: it writes a
// sibling temp file and renames it into place, so a reader never sees a
// half-written lockfile.
func Write(path string, s models.Selection) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal selection: %w", err)
	}
	data = append(data, '\n')

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

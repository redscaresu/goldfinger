package discovery

import (
	"sort"

	"github.com/redscaresu/goldfinger/models"
)

// Diff is the repo-level drift between a frozen selection and live discovery:
// repos that have appeared, disappeared, or whose default branch has moved since
// the lockfile was resolved. It is produced by Compare and reported by the check
// command; Compare never mutates the lockfile.
type Diff struct {
	Added              []models.Repo  // matches the filter now, absent from the lockfile
	Removed            []Removed      // in the lockfile, no longer selected (with a reason)
	DefaultBranchMoved []BranchChange // in both, but the recorded default branch changed
}

// Removed is a repo that was in the selection but is no longer selected by live
// discovery, annotated with the most likely reason.
type Removed struct {
	Repo   models.Repo
	Reason string
}

// BranchChange records a repo whose recorded default branch differs from the
// live one. This matters because apply's base routing falls back to the recorded
// default branch when no global --base-branch is set, so a silent move changes
// where a PR lands.
type BranchChange struct {
	Repo models.Repo
	Was  string
	Now  string
}

// Empty reports whether the selection is fully in sync at the repo level.
func (d Diff) Empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.DefaultBranchMoved) == 0
}

// Compare diffs a frozen selection against live discovery:
//   - locked is the repos recorded in the lockfile.
//   - live is today's repos that match the lockfile's filter (i.e. Select over
//     the raw set) — what a fresh select would record right now.
//   - raw is every repo the owner exposes today, pre-filter, used only to explain
//     WHY a locked repo is no longer selected (archived vs unmatched vs gone).
//
// All result slices are sorted by full name so output and tests are stable.
func Compare(locked, live, raw []models.Repo) Diff {
	lockedByName := indexByName(locked)
	liveByName := indexByName(live)
	rawByName := indexByName(raw)

	var d Diff
	// Added: selected now, but not recorded in the lockfile.
	for name, r := range liveByName {
		if _, ok := lockedByName[name]; !ok {
			d.Added = append(d.Added, r)
		}
	}
	// Removed and branch moves: walk the lockfile against the live selection.
	for name, was := range lockedByName {
		now, stillSelected := liveByName[name]
		if !stillSelected {
			d.Removed = append(d.Removed, Removed{Repo: was, Reason: removalReason(name, rawByName)})
			continue
		}
		// Only flag a branch move when both defaults are known. Older or
		// hand-written selections may omit the recorded default (apply's
		// resolveBase already tolerates that), so a blank side is "unknown",
		// not drift.
		if was.DefaultBranch != "" && now.DefaultBranch != "" && was.DefaultBranch != now.DefaultBranch {
			d.DefaultBranchMoved = append(d.DefaultBranchMoved, BranchChange{
				Repo: now,
				Was:  was.DefaultBranch,
				Now:  now.DefaultBranch,
			})
		}
	}

	sortByFullName(d.Added)
	sort.Slice(d.Removed, func(i, j int) bool {
		return d.Removed[i].Repo.FullName() < d.Removed[j].Repo.FullName()
	})
	sort.Slice(d.DefaultBranchMoved, func(i, j int) bool {
		return d.DefaultBranchMoved[i].Repo.FullName() < d.DefaultBranchMoved[j].Repo.FullName()
	})
	return d
}

// removalReason explains why a locked repo is no longer selected, using the raw
// (pre-filter) live set to distinguish the cases. A repo absent from raw may have
// been deleted, transferred, or simply be invisible to the current token, so the
// wording stays honest about that ambiguity.
func removalReason(name string, raw map[string]models.Repo) string {
	r, ok := raw[name]
	switch {
	case !ok:
		return "deleted, transferred, or no longer visible to this token"
	case r.Archived:
		return "archived"
	default:
		return "no longer matches filter"
	}
}

func indexByName(repos []models.Repo) map[string]models.Repo {
	m := make(map[string]models.Repo, len(repos))
	for _, r := range repos {
		m[r.FullName()] = r
	}
	return m
}

func sortByFullName(repos []models.Repo) {
	sort.Slice(repos, func(i, j int) bool { return repos[i].FullName() < repos[j].FullName() })
}

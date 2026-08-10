package apply

import (
	"bufio"
	"bytes"
	"io"
	"regexp"
	"strings"

	"github.com/redscaresu/goldfinger/models"
)

const (
	DryRunWouldChange = "would-change"
	DryRunNoChange    = "no-change"
	DryRunError       = "error"
	DryRunUnknown     = "unknown"
)

// DryRunDigest is the honest non-interactive dry-run signal multi-gitter gives:
// whether each selected repo would change, made no change, errored, or never
// appeared in multi-gitter's final repo counter block.
type DryRunDigest struct {
	RepoCount int
	Changed   int
	Unchanged int
	Errored   int
	Repos     []RepoDryRunStatus
	// Unparseable is set when none of multi-gitter's recognised result sections
	// appeared in the output, so the per-repo buckets can't be trusted (see
	// SummarizeDryRunOutput). A caller must not present a confident digest when
	// this is true — it means the format drifted (or, safely, every repo errored).
	Unparseable bool
}

// RepoDryRunStatus is one selected repo's dry-run outcome.
type RepoDryRunStatus struct {
	Repo   string
	Status string
	Detail string
}

// SummarizeDryRunOutput parses multi-gitter's final repocounter.Info() block.
// Non-interactive dry-run does not emit a unified diff or diffstat; these buckets
// are the only per-repo signal available.
func SummarizeDryRunOutput(repos []models.Repo, output []byte) DryRunDigest {
	digest := DryRunDigest{
		RepoCount: len(repos),
		Repos:     make([]RepoDryRunStatus, 0, len(repos)),
	}
	index := make(map[string]int, len(repos))
	for _, r := range repos {
		name := r.FullName()
		index[name] = len(digest.Repos)
		digest.Repos = append(digest.Repos, RepoDryRunStatus{
			Repo:   name,
			Status: DryRunUnknown,
		})
	}

	var bucketStatus, bucketDetail string
	sawKnownHeader := false
	reader := bufio.NewReader(bytes.NewReader(output))
	for {
		raw, err := reader.ReadString('\n')
		if raw != "" {
			parseDryRunLine(stripANSI(strings.TrimRight(raw, "\r\n")), index, digest.Repos, &bucketStatus, &bucketDetail, &sawKnownHeader)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			break
		}
	}

	for _, r := range digest.Repos {
		switch r.Status {
		case DryRunWouldChange:
			digest.Changed++
		case DryRunNoChange:
			digest.Unchanged++
		case DryRunError:
			digest.Errored++
		}
	}

	// Fail-safe against multi-gitter output drift. Every real run groups repos
	// under headers we recognise — at minimum a successful-run or no-change
	// section. If not one recognised header appeared, this isn't the format the
	// parser understands (a future multi-gitter may have reworked repocounter's
	// block), and the default bucket would otherwise relabel every listed repo as
	// "error" — a confident, wrong answer. Flag the whole digest unparseable so the
	// caller degrades honestly instead. This also (safely) trips on the rare
	// all-errors run, which carries no positive section: erring toward "unsure" is
	// the intended failure direction, never confident-but-wrong.
	if len(repos) > 0 && !sawKnownHeader {
		digest.Unparseable = true
	}
	return digest
}

func parseDryRunLine(line string, index map[string]int, statuses []RepoDryRunStatus, bucketStatus, bucketDetail *string, sawKnownHeader *bool) {
	if strings.HasPrefix(line, "  ") {
		if *bucketStatus == "" {
			return
		}
		repo := strings.TrimSpace(strings.TrimPrefix(line, "  "))
		repo = strings.TrimSuffix(repo, " #0")
		if i, ok := index[repo]; ok {
			statuses[i].Status = *bucketStatus
			statuses[i].Detail = *bucketDetail
		}
		return
	}

	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return
	}
	if !strings.HasPrefix(line, " ") && strings.HasSuffix(trimmed, ":") {
		header := strings.TrimSuffix(trimmed, ":")
		status, detail, known := dryRunBucket(header)
		*bucketStatus, *bucketDetail = status, detail
		if known {
			*sawKnownHeader = true
		}
	}
}

// dryRunBucket maps a multi-gitter section header to a per-repo status. known is
// true only for the headers goldfinger recognises by name (the successful-run and
// no-change sections); any other header falls to the error bucket AND reports
// known=false, which SummarizeDryRunOutput uses to detect a wholesale format
// drift (see its fail-safe). multi-gitter groups errored repos under the error
// text itself, so an unrecognised header is legitimately an error category — the
// digest is only distrusted when NO known header appears at all.
func dryRunBucket(header string) (status, detail string, known bool) {
	switch header {
	case "Repositories with a successful run":
		return DryRunWouldChange, "", true
	case "No data was changed":
		return DryRunNoChange, "", true
	default:
		return DryRunError, header, false
	}
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func stripANSI(s string) string {
	return ansiPattern.ReplaceAllString(s, "")
}

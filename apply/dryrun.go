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
	reader := bufio.NewReader(bytes.NewReader(output))
	for {
		raw, err := reader.ReadString('\n')
		if raw != "" {
			parseDryRunLine(stripANSI(strings.TrimRight(raw, "\r\n")), index, digest.Repos, &bucketStatus, &bucketDetail)
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
	return digest
}

func parseDryRunLine(line string, index map[string]int, statuses []RepoDryRunStatus, bucketStatus, bucketDetail *string) {
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
		*bucketStatus, *bucketDetail = dryRunBucket(header)
	}
}

func dryRunBucket(header string) (status, detail string) {
	switch header {
	case "Repositories with a successful run":
		return DryRunWouldChange, ""
	case "No data was changed":
		return DryRunNoChange, ""
	default:
		return DryRunError, header
	}
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func stripANSI(s string) string {
	return ansiPattern.ReplaceAllString(s, "")
}

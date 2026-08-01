#!/usr/bin/env bash
#
# End-to-end test for goldfinger against a dedicated sandbox repo.
#
# Exercises the whole pipeline and asserts each stage:
#   select --topic  -> isolates exactly the sandbox repo
#   mirror          -> clones it locally
#   apply --dry-run -> reports a change but pushes NOTHING
#   apply (real)    -> opens a real PR with the expected diff
# then tears down (closes the PR, deletes the branch) so the sandbox is left
# exactly as it started.
#
# NOT part of CI: it needs a real PAT and opens a real PR. Run it locally:
#   GOLD_FINGER_PAT=<pat> make e2e
#
# Requirements: go, gh (authenticated), jq. The sandbox repo must be tagged
# with the topic below and seeded with a README.md on its default branch.
set -euo pipefail

OWNER="${E2E_OWNER:-redscaresu}"
REPO="${E2E_REPO:-goldfinger-test}"
TOPIC="${E2E_TOPIC:-goldfinger-e2e}"
MARKER="goldfinger e2e marker"

: "${GOLD_FINGER_PAT:?GOLD_FINGER_PAT must be set (the PAT goldfinger uses)}"
for tool in go gh jq; do
	command -v "$tool" >/dev/null || { echo "FAIL: $tool is required" >&2; exit 1; }
done

TMP="$(mktemp -d)"
BRANCH="goldfinger-e2e-$$-$(date +%s)"
SELECTION="$TMP/goldfinger.selection"
GF="$TMP/goldfinger"

pr_number() {
	gh pr list --repo "$OWNER/$REPO" --head "$BRANCH" --state all --json number --jq '.[0].number // empty'
}

cleanup() {
	local pr
	pr="$(pr_number 2>/dev/null || true)"
	if [ -n "${pr:-}" ]; then
		gh pr close "$pr" --repo "$OWNER/$REPO" --delete-branch >/dev/null 2>&1 || true
	fi
	gh api -X DELETE "repos/$OWNER/$REPO/git/refs/heads/$BRANCH" >/dev/null 2>&1 || true
	rm -rf "$TMP"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

echo "==> building goldfinger"
go build -o "$GF" ./cmd

echo "==> select --topic $TOPIC (must isolate exactly $REPO)"
"$GF" select --org "$OWNER" --topic "$TOPIC" --selection "$SELECTION" >/dev/null
count="$(jq '.repos | length' "$SELECTION")"
[ "$count" = "1" ] || fail "expected 1 repo in selection, got $count"
jq -e --arg n "$REPO" '.repos[0].name == $n' "$SELECTION" >/dev/null || fail "selection is not $REPO"

echo "==> mirror"
"$GF" mirror --selection "$SELECTION" --workspace "$TMP/ws" >/dev/null
[ -d "$TMP/ws/$OWNER/$REPO/.git" ] || fail "mirror did not clone $REPO"

echo "==> apply --dry-run (must not push a branch)"
"$GF" apply --selection "$SELECTION" --branch "$BRANCH" \
	--commit-message "goldfinger e2e" --pr-title "goldfinger e2e" \
	-- sh -c "printf '%s\n' '$MARKER' >> README.md" >/dev/null
if gh api "repos/$OWNER/$REPO/git/refs/heads/$BRANCH" >/dev/null 2>&1; then
	fail "dry-run pushed branch $BRANCH — it must not"
fi

echo "==> apply --dry-run=false --confirm (opens a real PR)"
"$GF" apply --selection "$SELECTION" --branch "$BRANCH" \
	--commit-message "goldfinger e2e" --pr-title "goldfinger e2e" \
	--dry-run=false --confirm \
	-- sh -c "printf '%s\n' '$MARKER' >> README.md" >/dev/null

echo "==> verify PR"
pr="$(pr_number)"
[ -n "$pr" ] || fail "no PR was created for branch $BRANCH"
gh pr diff "$pr" --repo "$OWNER/$REPO" | grep -qF "$MARKER" || fail "PR #$pr diff is missing the marker"

echo "PASS: full pipeline opened PR #$pr on $OWNER/$REPO with the expected diff"
echo "==> teardown (closing PR #$pr, deleting branch $BRANCH)"

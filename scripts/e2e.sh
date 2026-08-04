#!/usr/bin/env bash
#
# End-to-end test for goldfinger against a dedicated sandbox repo.
#
# Exercises the whole pipeline and asserts each stage:
#   select --topic   -> isolates exactly the sandbox repo
#   mirror           -> clones it locally
#   mirror --purpose -> clones into an ephemeral, date-stamped ~/goldfinger/<purpose>-<date>
#   apply --dry-run  -> reports a change but pushes NOTHING
#   apply (real)     -> opens a real PR with the expected diff
# then tears down (closes the PR, deletes the branch, removes the --purpose dir)
# so the sandbox and home dir are left exactly as they started.
#
# NOT part of CI: it needs a real PAT and opens a real PR. Run it locally:
#   GOLD_FINGER_PAT=<pat> make e2e
#
# Requirements: go, gh (authenticated), jq, git. The sandbox repo must be tagged
# with the topic below and seeded with a README.md on its default branch.
set -euo pipefail

OWNER="${E2E_OWNER:-redscaresu}"
REPO="${E2E_REPO:-goldfinger-test}"
TOPIC="${E2E_TOPIC:-goldfinger-e2e}"
MARKER="goldfinger e2e marker"

: "${GOLD_FINGER_PAT:?GOLD_FINGER_PAT must be set (the PAT goldfinger uses)}"
for tool in go gh jq git; do
	command -v "$tool" >/dev/null || { echo "FAIL: $tool is required" >&2; exit 1; }
done

TMP="$(mktemp -d)"
BRANCH="goldfinger-e2e-$$-$(date +%s)"
TESTBRANCH="goldfinger-e2e-branch-$$-$(date +%s)"
SELECTION="$TMP/goldfinger.selection"
GF="$TMP/goldfinger"
# --purpose resolves under $HOME/goldfinger (os.UserHomeDir). We point HOME at a
# temp dir for those steps (below) so the e2e never depends on or writes to the
# runner's real home — it runs in CI where the home path isn't ours to assume.
PURPOSE="gfe2e$$"
BPURPOSE="gfe2eb$$"
PHOME="$TMP/home"

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
	# The throwaway ref created to prove --branch checks out.
	gh api -X DELETE "repos/$OWNER/$REPO/git/refs/heads/$TESTBRANCH" >/dev/null 2>&1 || true
	# Removes the temp workspace AND the --purpose clones under $PHOME (goldfinger
	# never deletes a --purpose dir itself), since $PHOME lives inside $TMP.
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

echo "==> mirror --purpose (ephemeral, timestamped <home>/goldfinger/<purpose>-<stamp>)"
mkdir -p "$PHOME"
# GOLD_FINGER_PAT is set, so goldfinger/ghorg get the token from env (not gh),
# and a clone needs no git identity — HOME can safely point at the temp dir.
HOME="$PHOME" "$GF" mirror --selection "$SELECTION" --purpose "$PURPOSE" >/dev/null
# goldfinger stamps the time to the millisecond, so we can't predict the exact
# name — match by glob on the purpose prefix.
PDIR="$(find "$PHOME/goldfinger" -maxdepth 1 -type d -name "$PURPOSE-*" 2>/dev/null | head -1)"
[ -n "$PDIR" ] || fail "--purpose did not create <home>/goldfinger/$PURPOSE-<stamp>"
case "$PDIR" in
	*-[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]-[0-9][0-9][0-9][0-9][0-9][0-9].[0-9][0-9][0-9]) : ;;
	*) fail "--purpose dir '$PDIR' is not timestamped YYYY-MM-DD-HHMMSS.mmm" ;;
esac
[ -d "$PDIR/$OWNER/$REPO/.git" ] || fail "--purpose workspace missing clone $OWNER/$REPO"

echo "==> mirror --workspace + --purpose are mutually exclusive"
if "$GF" mirror --selection "$SELECTION" --workspace "$TMP/ws" --purpose "$PURPOSE" >/dev/null 2>&1; then
	fail "--workspace and --purpose together should be rejected"
fi

echo "==> mirror --branch (checks out the named branch; folds it into --purpose dir name)"
# Create a throwaway ref off the default-branch head so the checkout is provable:
# without --branch ghorg would land on the default, so pointing --branch at a
# DISTINCT branch name (even at the same SHA) proves the flag took effect.
DEFAULT_BRANCH="$(jq -r '.repos[0].defaultBranch' "$SELECTION")"
[ -n "$DEFAULT_BRANCH" ] && [ "$DEFAULT_BRANCH" != "null" ] || fail "selection has no defaultBranch"
HEAD_SHA="$(gh api "repos/$OWNER/$REPO/git/refs/heads/$DEFAULT_BRANCH" --jq '.object.sha')"
[ -n "$HEAD_SHA" ] || fail "could not resolve $DEFAULT_BRANCH head sha"
gh api -X POST "repos/$OWNER/$REPO/git/refs" \
	-f ref="refs/heads/$TESTBRANCH" -f sha="$HEAD_SHA" >/dev/null || fail "could not create $TESTBRANCH"
HOME="$PHOME" "$GF" mirror --selection "$SELECTION" --purpose "$BPURPOSE" --branch "$TESTBRANCH" >/dev/null
# --purpose + --branch names the dir <purpose>-<branch>-<stamp>.
BDIR="$(find "$PHOME/goldfinger" -maxdepth 1 -type d -name "$BPURPOSE-$TESTBRANCH-*" 2>/dev/null | head -1)"
[ -n "$BDIR" ] || fail "--purpose --branch did not create <home>/goldfinger/$BPURPOSE-$TESTBRANCH-<stamp>"
[ -d "$BDIR/$OWNER/$REPO/.git" ] || fail "--branch workspace missing clone $OWNER/$REPO"
HEAD_BRANCH="$(git -C "$BDIR/$OWNER/$REPO" rev-parse --abbrev-ref HEAD)"
[ "$HEAD_BRANCH" = "$TESTBRANCH" ] || fail "--branch: checked-out branch is '$HEAD_BRANCH', want '$TESTBRANCH'"

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

# goldfinger demo — regenerate with:  vhs docs/demo.tape
#
# A LIVE recording: it really resolves, clones, scans, and dry-run-applies across
# a handful of public github.com/redscaresu repos. The apply is a DRY RUN, so it
# opens no PRs. Needs a gh auth session (or GOLD_FINGER_PAT), plus ghorg,
# multi-gitter, and perl on PATH.

Output docs/demo.gif

Set Shell "bash"
Set FontSize 17
Set Width 1240
Set Height 860
Set Theme "Dracula"
Set TypingSpeed 30ms
Set Margin 16
Set BorderRadius 8

# Clean prompt, a fresh working dir, and GHORG_QUIET to drop ghorg's figlet
# banner (setup hidden from the GIF; the commands below are the real thing).
Hide
Type "unset GITHUB_TOKEN GH_TOKEN; export PS1='$ ' GHORG_QUIET=true; rm -rf /tmp/gf-demo; mkdir -p /tmp/gf-demo; cd /tmp/gf-demo; clear"
Enter
Sleep 1s
Show

# 1. Freeze the target set into a reviewable lockfile.
Type "goldfinger select --org redscaresu --repo microblog --repo usersAPI --repo simpleAPI --repo weatherapp --repo webcrawler"
Enter
Sleep 3s

# 2. Clone them locally (so reads cost no API quota).
Type "goldfinger mirror"
Enter
Sleep 8s

# 3. Read the fleet: regex over the local clones, zero GitHub rate limit.
Type `goldfinger scan "go 1\."`
Enter
Sleep 3s

# 4. Dry-run a change across the set — opens nothing. See the per-repo digest.
Type `goldfinger apply --branch bump-go-124 --commit-message 'Bump Go to 1.24' --pr-title 'Bump Go to 1.24' --sign local -- perl -pi -e 's/^go 1\.(16|17)$/go 1.24/' go.mod`
Enter
Sleep 16s

Sleep 3s

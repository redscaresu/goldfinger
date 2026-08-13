package selection

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/redscaresu/goldfinger/models"
)

// digestHashLen is how many hex characters of the sha256 the short fingerprint
// keeps. 12 hex chars (48 bits) makes an accidental collision between two
// different fleets negligible for this purpose while staying glanceable on one
// line — it is a change-detector, not a cryptographic commitment.
const digestHashLen = 12

// Digest returns a stable fingerprint of a selection's repo SET: the repo count
// plus a short hash over the sorted "owner/name" full-names. Sorting makes it
// order-independent, so two selections covering the same repos — regardless of
// the order discovery happened to return them — produce the same hash. It covers
// only the repo identities, deliberately not branch-presence or provenance, so
// it answers exactly the question WS6 poses: "same N repos?". An agent can
// compare two digests to confirm a selection is unchanged without reading the
// whole lockfile back.
// SelectionBytesDigest is the full sha256, hex-encoded, of the exact bytes of a
// lockfile. Unlike Digest — a short fingerprint of the repo SET, meant for a
// human glance — this commits to the whole file content (every field, exact
// formatting), so it is the file's identity, not a semantic summary. It backs
// `apply --expect-selection-sha256 <hex>`: bind a real run to the precise
// lockfile a plan was reviewed against, and refuse if anything changed. Callers
// should feed it the same buffer they parsed (see ReadWithDigest) so the digest
// and the parsed selection cannot describe different content.
func SelectionBytesDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func Digest(sel models.Selection) (count int, hash string) {
	names := make([]string, len(sel.Repos))
	for i, r := range sel.Repos {
		names[i] = r.FullName()
	}
	sort.Strings(names)
	sum := sha256.Sum256([]byte(strings.Join(names, "\n")))
	return len(sel.Repos), hex.EncodeToString(sum[:])[:digestHashLen]
}

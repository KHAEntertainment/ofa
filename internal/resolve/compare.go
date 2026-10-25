package resolve

import (
	"strconv"
	"strings"
)

// isNewer reports whether latest is strictly newer than cur for the practical
// case of OFA.
//
// It understands semver-shaped versions: a numeric dotted core, an optional
// pre-release, and build metadata (ignored). A stable release outranks any
// pre-release of the same core, and pre-release identifiers compare
// numerically where they are numeric, so `0.0.0-beta-17823` <
// `0.0.0-beta-18743`. The same numeric core compare covers date-shaped
// versions like `2026.09.18` < `2026.10.0`, where lex order is wrong.
//
// Returns false when cur == latest or when the comparison is inconclusive (a
// non-numeric core segment, or two pre-release tags that are not comparable —
// `alpha` vs `beta`, or two build hashes). That asymmetry is deliberate: OFA
// only shows "outdated" as actionable advice, so false negatives are
// recoverable (re-run later) while false positives are noise.
func isNewer(cur, latest string) bool {
	if cur == "" || latest == "" {
		return false
	}
	cur = stripBrewRev(cur)
	latest = stripBrewRev(latest)
	if cur == latest {
		return false
	}

	cv, ok := parseVersion(cur)
	if !ok {
		return false
	}
	lv, ok := parseVersion(latest)
	if !ok {
		return false
	}
	return cv.less(lv)
}

// version is a parsed version: numeric core segments plus pre-release
// identifiers (empty for a stable release).
type version struct {
	core []int
	pre  []string
}

// parseVersion splits a version into its core and pre-release. Build metadata
// after '+' is discarded (semver says it carries no precedence). Returns false
// when a core segment is not a non-negative integer, so callers stay
// conservative instead of guessing.
func parseVersion(v string) (version, bool) {
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	core, pre, hasPre := strings.Cut(v, "-")

	segments := strings.Split(core, ".")
	nums := make([]int, 0, len(segments))
	for _, s := range segments {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return version{}, false
		}
		nums = append(nums, n)
	}
	if !hasPre {
		return version{core: nums}, true
	}
	if pre == "" {
		return version{}, false
	}

	// Both '.' and '-' separate identifiers here: semver only defines '.', but
	// the harnesses in the Catalog emit forms like `0.0.0-beta-18743`, where
	// the build number after the hyphen is the part that advances.
	ids := strings.FieldsFunc(pre, func(r rune) bool { return r == '.' || r == '-' })
	if len(ids) == 0 {
		return version{}, false
	}
	return version{core: nums, pre: ids}, true
}

// less reports whether v is strictly older than o.
func (v version) less(o version) bool {
	n := len(v.core)
	if len(o.core) > n {
		n = len(o.core)
	}
	for i := 0; i < n; i++ {
		a, b := coreAt(v.core, i), coreAt(o.core, i)
		if a != b {
			return a < b
		}
	}

	switch {
	case len(v.pre) == 0 && len(o.pre) == 0:
		return false
	case len(v.pre) == 0:
		return false // stable beats the other's pre-release: v is newer
	case len(o.pre) == 0:
		return true // v is a pre-release of o's stable core
	}
	return prereleaseLess(v.pre, o.pre)
}

// coreAt returns the i-th core segment, zero-padded so trailing segments are
// optional: 1.2 and 1.2.0 are the same version.
func coreAt(core []int, i int) int {
	if i < len(core) {
		return core[i]
	}
	return 0
}

// prereleaseLess orders pre-release identifier lists. It answers only when the
// shared prefix is identical: numeric identifiers compare numerically, equal
// tag names are skipped, and the first differing non-numeric pair is
// inconclusive (false). A strict semver reading would order alpha < beta and
// two build hashes lexically, but neither ordering means anything for OFA's
// "is there a newer release" question, and guessing would produce noise.
func prereleaseLess(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		an, aNum := numericIdent(a[i])
		bn, bNum := numericIdent(b[i])
		switch {
		case aNum && bNum:
			if an != bn {
				return an < bn
			}
		case a[i] != b[i]:
			return false
		}
	}

	if len(a) == len(b) {
		return false
	}

	// Same shared prefix, different length: `beta` precedes `beta.2` only when
	// the extra identifier is numeric (semver's rule). Anything else is
	// inconclusive rather than a guess.
	longer := a
	if len(b) > len(a) {
		longer = b
	}
	shared := len(a)
	if len(b) < shared {
		shared = len(b)
	}
	if _, ok := numericIdent(longer[shared]); !ok {
		return false
	}
	return len(a) < len(b)
}

// numericIdent parses a pre-release identifier as a non-negative integer.
func numericIdent(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

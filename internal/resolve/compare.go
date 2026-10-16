package resolve

import (
	"strconv"
	"strings"
)

// isNewer reports whether latest is strictly newer than cur for the
// practical case of OFA. Semver-aware where the input is semver-shaped;
// falls back to numeric dotted-segment compare for date-shaped versions
// like "2026.09.18" (cursor-agent) where lex order is wrong ("2026.10.0"
// < "2026.9.1" lexically but newer numerically).
//
// Returns false when cur == latest or when comparison is inconclusive
// (a malformed segment), so a failure to parse never marks an install
// outdated. The trade-off is the inverse of what we'd want for security
// advisories, but OFA only shows "outdated" as actionable advice — false
// negatives are recoverable (re-run later), false positives are noisy.
func isNewer(cur, latest string) bool {
	if cur == "" || latest == "" {
		return false
	}
	cur = stripBrewRev(cur)
	latest = stripBrewRev(latest)
	if cur == latest {
		return false
	}
	cs, lok := dotted(cur)
	ls, lok2 := dotted(latest)
	if !lok || !lok2 {
		// unparseable: don't claim it's newer
		return false
	}
	// pad shorter to same length with zeros so trailing .0 doesn't lose
	for len(cs) < len(ls) {
		cs = append(cs, 0)
	}
	for len(ls) < len(cs) {
		ls = append(ls, 0)
	}
	for i := range cs {
		if ls[i] > cs[i] {
			return true
		}
		if ls[i] < cs[i] {
			return false
		}
	}
	return false
}

// dotted parses a version into numeric segments. Returns (nil, false) when
// any segment is non-numeric (pre-release tags, suffixes, dates with month
// names, etc.) so isNewer falls back to the conservative answer.
func dotted(v string) ([]int, bool) {
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}
// Package semver orders Versola release tags ("0.7.5", "0.8.0-rc1"), which
// have no leading "v". Two things need it: the update notice (is the latest
// release newer than the one installed) and the compatibility manifest (is
// the version that runs lower than the one being deployed).
package semver

import (
	"strconv"
	"strings"
)

// Parse splits a release tag ("0.3.3", "0.4.0-rc1", with an optional leading
// "v") into its numeric core and its pre-release identifiers. ok is false for
// anything else; in particular only [0-9A-Za-z.-] get through, so a tag that
// parsed is safe to print and to put in a URL.
func Parse(s string) (core [3]uint64, pre []string, ok bool) {
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return core, nil, false
	}
	main, preStr, hasPre := strings.Cut(s, "-")
	parts := strings.Split(main, ".")
	if len(parts) != 3 {
		return core, nil, false
	}
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil || p == "" || (len(p) > 1 && p[0] == '0') {
			return core, nil, false
		}
		core[i] = n
	}
	if !hasPre {
		return core, nil, true
	}
	pre = strings.Split(preStr, ".")
	for _, id := range pre {
		if id == "" {
			return core, nil, false
		}
		for _, r := range id {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-') {
				return core, nil, false
			}
		}
	}
	return core, pre, true
}

// Compare orders two release tags like semver does: the core numerically, a
// pre-release below the release it precedes, pre-release identifiers one by
// one (numbers below words, fewer below more). ok is false when either does
// not parse.
func Compare(a, b string) (cmp int, ok bool) {
	ac, ap, aok := Parse(a)
	bc, bp, bok := Parse(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range ac {
		if ac[i] != bc[i] {
			if ac[i] < bc[i] {
				return -1, true
			}
			return 1, true
		}
	}
	switch {
	case len(ap) == 0 && len(bp) == 0:
		return 0, true
	case len(ap) == 0:
		return 1, true
	case len(bp) == 0:
		return -1, true
	}
	for i := 0; i < len(ap) && i < len(bp); i++ {
		if c := comparePreIdentifier(ap[i], bp[i]); c != 0 {
			return c, true
		}
	}
	switch {
	case len(ap) < len(bp):
		return -1, true
	case len(ap) > len(bp):
		return 1, true
	}
	return 0, true
}

func comparePreIdentifier(a, b string) int {
	an, aerr := strconv.ParseUint(a, 10, 64)
	bn, berr := strconv.ParseUint(b, 10, 64)
	switch {
	case aerr == nil && berr == nil:
		if an < bn {
			return -1
		}
		if an > bn {
			return 1
		}
		return 0
	case aerr == nil:
		return -1
	case berr == nil:
		return 1
	}
	return strings.Compare(a, b)
}

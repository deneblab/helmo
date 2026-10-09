package registry

import (
	"regexp"
	"sort"
	"strconv"
)

var semverTag = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

type version struct {
	tag string
	n   [3]int
}

// IsVersionTag reports whether tag is X.Y.Z or vX.Y.Z.
func IsVersionTag(tag string) bool { return semverTag.MatchString(tag) }

// SortVersions returns the version tags of in, newest first. Other tags are
// dropped. When 1.2.3 and v1.2.3 both exist both stay, "v" first.
func SortVersions(in []string) []string {
	vs := make([]version, 0, len(in))
	for _, t := range in {
		m := semverTag.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		v := version{tag: t}
		for i := range v.n {
			v.n[i], _ = strconv.Atoi(m[i+1])
		}
		vs = append(vs, v)
	}
	sort.Slice(vs, func(i, j int) bool {
		for k := range vs[i].n {
			if vs[i].n[k] != vs[j].n[k] {
				return vs[i].n[k] > vs[j].n[k]
			}
		}
		return vs[i].tag > vs[j].tag
	})
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.tag
	}
	return out
}

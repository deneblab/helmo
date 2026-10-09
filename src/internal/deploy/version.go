// Package deploy changes the version of an app: it pins the image in
// .helmo/env, pulls and recreates the service, waits for it to be healthy
// and rolls back when it is not.
package deploy

import (
	"fmt"
	"regexp"
	"strings"

	"helmo/internal/registry"
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Version is what APP_TAG holds: a tag, pinned to the digest the registry
// served when it was deployed ("v1.2.3@sha256:..."). The digest makes a
// rollback return to the exact image even if the tag was moved since.
type Version struct {
	Tag    string
	Digest string
}

func (v Version) String() string {
	if v.Digest == "" {
		return v.Tag
	}
	return v.Tag + "@" + v.Digest
}

func (v Version) IsZero() bool { return v.Tag == "" && v.Digest == "" }

// NewVersion validates a version Helmo is about to write.
func NewVersion(tag, digest string) (Version, error) {
	if !registry.IsVersionTag(tag) {
		return Version{}, fmt.Errorf("%q is not a version tag (X.Y.Z or vX.Y.Z)", tag)
	}
	if !digestPattern.MatchString(digest) {
		return Version{}, fmt.Errorf("invalid digest %q", digest)
	}
	return Version{Tag: tag, Digest: digest}, nil
}

// parseCurrent reads a stored APP_TAG leniently: it may be any tag a human
// put there.
func parseCurrent(s string) Version {
	tag, digest, _ := strings.Cut(strings.TrimSpace(s), "@")
	return Version{Tag: tag, Digest: digest}
}

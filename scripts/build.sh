#!/bin/sh
# Builds the Helmo image, versioned by abcversion from the git history.
#
#   scripts/build.sh                 -> ghcr.io/deneblab/helmo:<version>
#   IMAGE=example/helmo scripts/build.sh
#
# Pushing is left to you: docker push "$IMAGE:<version>".
set -eu

cd "$(dirname "$0")/.."

if ! command -v abcversion >/dev/null 2>&1; then
    echo "abcversion is not installed: https://github.com/deneblab/abcversion" >&2
    exit 1
fi

VERSION=$(abcversion -p semversion)
IMAGE=${IMAGE:-ghcr.io/deneblab/helmo}

echo "building $IMAGE:$VERSION"
docker build --build-arg "VERSION=$VERSION" -t "$IMAGE:$VERSION" .
echo "built $IMAGE:$VERSION"

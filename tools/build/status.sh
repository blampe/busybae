#!/usr/bin/env bash
# Bazel workspace-status script. Every line emitted here becomes a key
# available to x_defs / --stamp. Keys prefixed with STABLE_ influence the
# action cache; all other keys go to volatile-status.txt and don't
# invalidate cached actions.
#
# See https://bazel.build/docs/user-manual#workspace-status
set -euo pipefail

# Prefer the exact tag when we're on one; fall back to a dev descriptor.
if version=$(git describe --tags --always --dirty --match 'v*' 2>/dev/null); then
    :
else
    version="dev"
fi

# Trim the leading `v` on tags so consumers see "0.1.2" not "v0.1.2"; the
# release-URL builder in internal/wrapper re-adds the prefix.
version="${version#v}"

echo "STABLE_BUSYBAE_VERSION ${version}"
echo "STABLE_BUSYBAE_COMMIT $(git rev-parse HEAD 2>/dev/null || echo unknown)"

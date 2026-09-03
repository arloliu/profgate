#!/usr/bin/env bash
# Print the GitHub Release notes for one tag on stdout.
#
# The notes are that version's section of CHANGELOG.md with its headings promoted one level,
# then the image and chart the release workflow publishes for the tag,
# then a link to the changelog at that tag.
# .github/workflows/release.yml runs this after publishing and hands the output to `gh release create`.
# Run by hand, it writes the same notes for a tag that has no release yet:
#
#   scripts/release-notes.sh v0.5.0 sha256:... | gh release create v0.5.0 --verify-tag --title v0.5.0 --notes-file -
#
# Usage: scripts/release-notes.sh vX.Y.Z [image-digest]
# IMAGE_REPO, CHARTS_REPO, and REPO override the GHCR and GitHub paths the
# notes name.

set -euo pipefail

fail() {
	printf 'release-notes: %s\n' "$1" >&2
	exit 1
}

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
	fail "expected a version and an optional image digest, got $# arguments. Usage: scripts/release-notes.sh vX.Y.Z [image-digest]"
fi

version="$1"
digest="${2:-}"

if ! printf '%s' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
	fail "version '$version' is not vX.Y.Z, for example v0.2.0"
fi

image_repo="${IMAGE_REPO:-ghcr.io/arloliu/profgate}"
charts_repo="${CHARTS_REPO:-ghcr.io/arloliu/charts}"
repo="${REPO:-arloliu/profgate}"
chart_version="${version#v}"

# The section runs from the version's own heading to the next version heading.
# The link reference lines at the end of the file are not part of any section.
section="$(awk -v heading="## [$chart_version]" '
	index($0, heading) == 1 { found = 1; next }
	found && /^## \[/ { exit }
	found && /^\[[^]]+\]: / { next }
	found { print }
' CHANGELOG.md | sed -e 's/^### /## /' -e '/./,$!d')"

if [ -z "$section" ]; then
	fail "CHANGELOG.md has no section headed '## [$chart_version]'"
fi

printf '%s\n\n## Artifacts\n\n' "$section"
printf -- '- Image: `%s:%s`\n' "$image_repo" "$version"
if [ -n "$digest" ]; then
	printf -- '- Image digest: `%s@%s`\n' "$image_repo" "$digest"
fi
printf -- '- Chart: `oci://%s/profgate` version `%s`\n\n' "$charts_repo" "$chart_version"
printf '```sh\nhelm install profgate oci://%s/profgate --version %s\n```\n\n' "$charts_repo" "$chart_version"
printf '**Full changelog**: [CHANGELOG.md](https://github.com/%s/blob/%s/CHANGELOG.md)\n' "$repo" "$version"

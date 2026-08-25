#!/usr/bin/env bash
# Refuse to cut a release tag unless the repository is ready for one.
#
# The tag is the trigger: pushing v* starts .github/workflows/release.yml,
# which runs the unit gates and the current end-to-end lane again and then
# publishes the image to ghcr.io/arloliu/profgate and the chart to
# oci://ghcr.io/arloliu/charts. This script publishes nothing. It checks what
# a person cannot see at a glance -- an unpushed commit, a tag that already
# exists, a red run on the commit being tagged -- and only then creates the
# annotated tag and pushes it.
#
# Run via: mise run release -- vX.Y.Z

set -euo pipefail

remote=origin
branch=main

fail() {
	printf 'release: %s\n' "$1" >&2
	exit 1
}

if [ "$#" -ne 1 ]; then
	fail "expected one version argument, got $#. Usage: mise run release -- vX.Y.Z"
fi

version="$1"

if ! printf '%s' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
	fail "version '$version' is not vX.Y.Z, for example v0.2.0"
fi

dirty="$(git status --porcelain)"
if [ -n "$dirty" ]; then
	printf 'release: the working tree is dirty; commit or stash first:\n%s\n' "$dirty" >&2
	exit 1
fi

git fetch --quiet --tags "$remote" "$branch"

head_sha="$(git rev-parse HEAD)"
remote_sha="$(git rev-parse "$remote/$branch")"
if [ "$head_sha" != "$remote_sha" ]; then
	fail "HEAD $head_sha is not $remote/$branch $remote_sha; push or pull first"
fi

if git rev-parse -q --verify "refs/tags/$version" >/dev/null; then
	fail "tag $version already exists locally"
fi
if [ -n "$(git ls-remote --tags "$remote" "refs/tags/$version")" ]; then
	fail "tag $version already exists on $remote"
fi

# The release workflow reruns these gates on the tag, so a red run here is an
# early warning rather than the only barrier. Take the newest run per workflow
# on main for this commit: a rerun replaces its predecessor in this listing.
for workflow in check e2e; do
	line="$(gh run list --workflow "$workflow" --commit "$head_sha" --branch "$branch" \
		--limit 1 --json status,conclusion,url \
		--jq '.[] | "\(.status)\t\(.conclusion)\t\(.url)"')"

	if [ -z "$line" ]; then
		fail "no $workflow run for $head_sha on $branch.
The e2e lane is not started at all for a push that touches only
documentation and Markdown, so such a commit can never satisfy this check."
	fi

	status="$(printf '%s' "$line" | cut -f1)"
	conclusion="$(printf '%s' "$line" | cut -f2)"
	url="$(printf '%s' "$line" | cut -f3)"

	if [ "$status" != "completed" ]; then
		fail "the $workflow run for $head_sha is $status, not finished.
Wait for $url and run this again."
	fi
	if [ "$conclusion" != "success" ]; then
		fail "the $workflow run for $head_sha concluded $conclusion: $url"
	fi
done

git tag -a "$version" -m "profgate $version"
git push --quiet "$remote" "refs/tags/$version"

printf 'release: tagged and pushed %s at %s\n' "$version" "$head_sha"
printf 'release: https://github.com/%s/actions/workflows/release.yml\n' \
	"$(gh repo view --json nameWithOwner --jq .nameWithOwner)"

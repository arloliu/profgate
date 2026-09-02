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
# The e2e workflow ignores docs/**, .agents/**, *.md, AGENTS.md, CLAUDE.md,
# and LICENSE,
# so a commit that changes only those paths gets no e2e run of its own.
# For e2e only, this script walks HEAD's first-parent ancestors when HEAD
# has no run of its own.
# It looks for the nearest ancestor that does have an e2e run on main.
# It accepts that ancestor's run in HEAD's place once every path HEAD
# changed since then falls inside the ignored set.
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

# Mirrors .github/workflows/e2e.yml's paths-ignore list for both triggers.
e2e_ignored_path() {
	case "$1" in
	docs/* | .agents/* | *.md | AGENTS.md | CLAUDE.md | LICENSE) return 0 ;;
	*) return 1 ;;
	esac
}

# Find an ancestor's e2e run that HEAD can stand in for.
# Walks $1's first-parent ancestors on $branch, nearest first, skipping $1
# itself and stopping after 50 commits, for the first one with an e2e run.
# That run only counts as a stand-in when every path changed between it and $1 is one that e2e_ignored_path accepts.
# A commit with real changes but no run of its own is a different problem this does not paper over.
# Prints "<ancestor-sha>\t<status>\t<conclusion>\t<url>" and returns 0 on a
# match; prints nothing and returns 1 otherwise.
find_inheritable_e2e_run() {
	local head="$1" sha line changed path
	for sha in $(git rev-list --first-parent -n 51 "$head" | tail -n +2); do
		line="$(gh run list --workflow e2e --commit "$sha" --branch "$branch" \
			--limit 1 --json status,conclusion,url \
			--jq '.[] | "\(.status)\t\(.conclusion)\t\(.url)"')"
		[ -n "$line" ] || continue

		changed="$(git diff --name-only "$sha" "$head")"
		[ -n "$changed" ] || return 1

		while IFS= read -r path; do
			e2e_ignored_path "$path" || return 1
		done <<<"$changed"

		printf '%s\t%s\n' "$sha" "$line"
		return 0
	done
	return 1
}

# The release workflow reruns these gates on the tag, so a red run here is an
# early warning rather than the only barrier. Take the newest run per workflow
# on main for this commit: a rerun replaces its predecessor in this listing.
for workflow in check e2e; do
	run_sha="$head_sha"
	line="$(gh run list --workflow "$workflow" --commit "$run_sha" --branch "$branch" \
		--limit 1 --json status,conclusion,url \
		--jq '.[] | "\(.status)\t\(.conclusion)\t\(.url)"')"

	if [ -z "$line" ] && [ "$workflow" = e2e ] && inherited="$(find_inheritable_e2e_run "$head_sha")"; then
		run_sha="$(printf '%s' "$inherited" | cut -f1)"
		line="$(printf '%s' "$inherited" | cut -f2-4)"
		printf 'release: accepting the e2e run for %s in place of %s -- every path changed since then is one the e2e workflow ignores.\n' \
			"$run_sha" "$head_sha"
	fi

	if [ -z "$line" ]; then
		message="no $workflow run for $head_sha on $branch"
		if [ "$workflow" = e2e ]; then
			# The workflow's paths-ignore keeps GitHub from creating a run on
			# a docs-only commit, so waiting for one never ends.
			# This script looks for an ancestor's run to inherit instead;
			# it found none whose diff to HEAD stays inside the ignored paths.
			message="$message.
No ancestor's e2e run applies: either none of HEAD's last 50 first-parent
ancestors on $branch has an e2e run, or the nearest one that does differs
from HEAD by more than documentation and Markdown."
		fi
		fail "$message"
	fi

	status="$(printf '%s' "$line" | cut -f1)"
	conclusion="$(printf '%s' "$line" | cut -f2)"
	url="$(printf '%s' "$line" | cut -f3)"

	if [ "$status" != "completed" ]; then
		fail "the $workflow run for $run_sha is $status, not finished.
Wait for $url and run this again."
	fi
	if [ "$conclusion" != "success" ]; then
		fail "the $workflow run for $run_sha concluded $conclusion: $url"
	fi
done

git tag -a "$version" -m "profgate $version"
git push --quiet "$remote" "refs/tags/$version"

printf 'release: tagged and pushed %s at %s\n' "$version" "$head_sha"
printf 'release: https://github.com/%s/actions/workflows/release.yml\n' \
	"$(gh repo view --json nameWithOwner --jq .nameWithOwner)"

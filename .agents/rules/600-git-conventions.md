# 600 - Git Conventions

Apply when crafting commits, branches, or PR titles and descriptions.

## Branches

Prefixes: `feat/`, `fix/`, `docs/`, `chore/`, `test/`, `refactor/`, `perf/`.

## Commit Messages

The `commit-msg` hook in `.githooks/` refuses a message that breaks the three measurable rules below;
`mise run hooks` installs it once per clone ([500](500-validation-and-workflow.md)).
Review enforces the rest.

- [Conventional Commits](https://www.conventionalcommits.org/) type prefix
  required: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`,
  `build`, `ci`, `chore`, `revert`.
  Optional scope: `fix(k8s): ...`.
  Present tense, imperative.
- Header under 50 characters.
- Body lines under 72 characters, broken at clause boundaries:
  one sentence per line, a long sentence split where a clause ends,
  never at a column.
  The hook runs `semlf check` on the message and refuses a fused line.
- Body explains what changed and why, at a level a reader gets nothing else
  from — the diff already lists the files.

### Write the Fact, Not the Process

`git log` and `git blame` readers cannot see the plan you were following.
Name the change; leave sequencing labels (`PR-1`, `Phase 4`), work-item and
session IDs, review-round references, and `tmp/*` paths out of the message.
A committed file path is fine to cite; its internal section or question
numbering is not.

- Bad: `fix(k8s): resolve endpoint bug per plan step 3`
- Good: `fix(k8s): skip endpoints whose ready condition is unset`

### Attribution

Commit messages and PR bodies carry the change and nothing else.
Leave out `Co-Authored-By`, "Generated with ...", and every other attribution
trailer.

## Pull Requests

Title matches the commit format.
Body restates why for reviewers who have not read the plan, leading with
domain language.
This repository uses GitHub; open PRs with `gh pr create`.

# 600 - Git Conventions

Apply when crafting commits, branches, or PR titles and descriptions.

## Branches

Prefixes: `feat/`, `fix/`, `docs/`, `chore/`, `test/`, `refactor/`, `perf/`.

## Commit Messages

No `commitlint` configuration exists yet — until one does, convention and
review enforce what follows.

- [Conventional Commits](https://www.conventionalcommits.org/) type prefix
  required: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`,
  `build`, `ci`, `chore`, `revert`.
  Optional scope: `fix(k8s): ...`.
  Present tense, imperative.
- Header under 50 characters; body lines under 72.
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

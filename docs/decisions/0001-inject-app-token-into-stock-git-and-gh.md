# 0001. Inject the App token into stock git and gh

- Status: Accepted
- Date: 2026-11-12

## Context
Agents (Claude Code and others) commit, push, and open PRs on a machine where the
human is already authenticated (`gh` keyring token, SSH keys, osxkeychain). Agent
work should be credited to a GitHub App and must never fall back to the human's
credentials. Agents already know how to use `git` and `gh`. The less new surface
they have to learn, the more reliably they'll use it.

## Decision
`ghapp` is a single Go binary with no dependencies. It mints GitHub App
installation tokens (JWT → `/app/installations/{id}/access_tokens`, cached until
5 minutes before expiry) and hands them to unmodified `git` and `gh`:

- `ghapp exec -- <cmd>` sets `GH_TOKEN` and the bot's author/committer identity.
  Through `GIT_CONFIG_COUNT`, it also adds a credential helper that first *clears*
  every earlier helper, plus `insteadOf` rules that rewrite SSH remotes to HTTPS
  so SSH keys are skipped.
- `ghapp setup-git` writes the same settings into one repo's `.git/config`.

## Rejected alternatives
- **Create commits through the GitHub API (Git Data / GraphQL `createCommitOnBranch`):**
  rejected because agents would need a custom commit command instead of `git`.
  It breaks on large or binary changes and on multi-commit histories, and local
  history drifts from what was pushed. Its one real advantage (commits shown as
  "Verified") is listed as a follow-up below.
- **Put a long-lived token in `GH_TOKEN` via Claude Code `settings.json` env:**
  rejected because installation tokens expire after 1 hour, so a fixed value goes
  stale mid-session. Minting on demand in the helper avoids that.
- **Shell script using `openssl` + `curl`:** rejected because the git
  credential-helper protocol, per-repo installation lookup, token caching and
  remote parsing all get fragile in shell, and shell is hard to test. Go's
  standard library covers RS256 JWTs with no dependencies and ships as one
  static binary.
- **Existing tools (e.g. `gh` extensions, `actions/create-github-app-token`):**
  rejected because they target CI or only print a token. None of them clear the
  human's credential helpers or set the bot identity for local agent sessions.

## Consequences
- Agents keep using normal `git`/`gh` commands, prefixed with `ghapp exec --`.
- Pushed commits are credited to `<slug>[bot]` but are **not signed**. They won't
  show as "Verified", and branches that require signed commits will reject them.
  Follow-up if needed: an API-based `ghapp commit` path, or SSH signing.
- `setup-git` lasts until undone: a human pushing from that repo also acts as the
  app until they run `ghapp setup-git --undo`.
- Changing workflow files (`.github/workflows/*`) requires the App to have the
  Workflows permission.

# 0002. A closed `read` command with reduced tokens

- Status: Accepted
- Date: 2026-11-23

## Context
Harmless PR reads through `ghapp exec -- gh api ...` were blocked by a permission
classifier. `exec` runs anything with the app's full token, so a permission rule
can't tell a read from a write. Missing app permissions also surfaced only as an
unexplained 403.

## Decision
`ghapp read` offers a fixed table of GET endpoints. Arguments are validated, no
command is run, and each call mints an uncached token limited to one repository
and one read permission. Failures in `exec` are explained by watching stderr when
it isn't a terminal.

## Rejected alternatives
- **`ghapp exec --read-only -- <cmd>`**: rejected because it still runs arbitrary
  commands locally, so allowing its prefix allows any program.
- **Generic `ghapp get <api-path>`**: rejected because the caller picks the
  endpoint, so the token would need every read permission on every repo, and new
  API surface would be allowed without review.
- **One token with all read permissions**: rejected because GitHub refuses to mint
  a token with a permission the app lacks, so one gap would break every read.
- **Cache read tokens**: rejected because one extra request is cheap, and not
  caching rules out confusion with `exec`'s full token.
- **Always watch stderr in `exec`**: rejected because a pipe in place of a terminal
  disables git progress and gh prompts for humans.

## Consequences
- `Bash(ghapp read *)` can be allowed on its own. New resources need a code change.
- Whether an agent's classifier accepts it is up to that agent's rules.
- On a terminal, `exec` gives no permission hint; `ghapp doctor` still does.
- The permission named in an `exec` hint is inferred from the command text.
- With a non-terminal stderr, stdout/stderr interleaving may shift slightly, and
  output from background children is dropped 2s after the command exits.

# ghapp

Make Claude Code (or any agent) commit, push, and open pull requests **as a GitHub App**
instead of as you.

`ghapp` mints short-lived installation tokens and hands them to the normal `git` and `gh`
you already have. Commits are authored by `<your-app>[bot]`, PRs are opened by the app, and
your keychain, `gh` login and SSH keys are bypassed rather than used as a fallback.

```sh
ghapp exec -- git push -u origin my-branch
ghapp exec -- gh pr create --fill
```

## 1. Create the GitHub App (one time)

1. Go to **Settings → Developer settings → GitHub Apps → New GitHub App**
   (or under an organization: *Org settings → Developer settings*).
2. Name it, e.g. `drhayt-agent`. This becomes the bot name `drhayt-agent[bot]`.
   Use any Homepage URL. **Uncheck "Webhook → Active".**
3. Repository permissions:
   | Permission     | Access         | Why                                     |
   |----------------|----------------|-----------------------------------------|
   | Contents       | Read and write | push branches                           |
   | Pull requests  | Read and write | open/update PRs                         |
   | Metadata       | Read           | required (default)                      |
   | Workflows      | Read and write | *only* if agents may edit `.github/workflows` |
4. *Where can this app be installed?* → **Only on this account**. Create it.
5. On the app page, note the **App ID**, then **Generate a private key**. A `.pem` file downloads.
6. **Install App** (left sidebar) → pick the repositories the agent is allowed to touch.

Store the key somewhere private:

```sh
mkdir -p ~/.config/ghapp && mv ~/Downloads/drhayt-agent.*.private-key.pem ~/.config/ghapp/key.pem
chmod 600 ~/.config/ghapp/key.pem
```

## 2. Install and configure

```sh
go install github.com/DrHayt/ghapp@latest   # or: go build -o ~/bin/ghapp .
cat > ~/.config/ghapp/config.json <<'EOF'
{ "app_id": "123456", "private_key_path": "~/.config/ghapp/key.pem" }
EOF
ghapp whoami        # → drhayt-agent[bot] <123+drhayt-agent[bot]@users.noreply.github.com>
```

Environment variables override the file: `GHAPP_APP_ID`, `GHAPP_PRIVATE_KEY_PATH`,
`GHAPP_INSTALLATION_ID` (skip the per-repo lookup), `GHAPP_API_URL` + `GHAPP_HOST`
(GitHub Enterprise Server), `GHAPP_CONFIG` (alternate config path).

## 3. Use it

| Command | What it does |
|---|---|
| `ghapp exec -- <cmd>` | Runs `<cmd>` with `GH_TOKEN`/`GITHUB_TOKEN`, the bot as git author and committer, a credential helper that **clears** your existing helpers, and SSH→HTTPS remote rewriting. Nothing persists after the command. |
| `ghapp setup-git` | Writes the same git settings into this repo's `.git/config`, so plain `git commit`/`git push` act as the app. Undo with `ghapp setup-git --undo`. (`gh` still needs `ghapp exec`.) |
| `ghapp token [owner/repo]` | Prints an installation token for scripts. |
| `ghapp whoami` | Prints the bot identity. |

Tokens last 1 hour. They're cached in your user cache dir (mode 0600) and refreshed automatically.
The installation is found from the `origin` remote of the current directory.

## 4. Wire it into Claude Code

Add to the project's `CLAUDE.md` (or `~/.claude/CLAUDE.md` to apply everywhere):

```markdown
## Git identity
Always act as the GitHub App, never as the user. Prefix every git command that
commits or talks to GitHub, and every `gh` command, with `ghapp exec --`:
- `ghapp exec -- git commit -m "..."`
- `ghapp exec -- git push -u origin <branch>`
- `ghapp exec -- gh pr create --title "..." --body "..."`
```

To enforce it rather than just ask, deny the bare commands in `.claude/settings.json`.
`ghapp exec -- ...` starts with `ghapp`, so it isn't blocked:

```json
{
  "permissions": {
    "allow": ["Bash(ghapp exec:*)"],
    "deny": ["Bash(git push:*)", "Bash(git commit:*)", "Bash(gh pr:*)", "Bash(gh api:*)"]
  }
}
```

Other agents work the same way: run their git/gh calls through `ghapp exec --`, or run
`ghapp setup-git` in the repo they work in.

## Limitations

- Commits are **not signed**, so GitHub won't show them as "Verified". Branches that require
  signed commits will reject the push. See [ADR 0001](docs/decisions/0001-inject-app-token-into-stock-git-and-gh.md).
- The app can only reach repos it is installed on, with the permissions you granted. That's the point.

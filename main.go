// Command ghapp lets agents commit, push, and open pull requests as a GitHub App
// instead of as the human whose machine they run on.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const usage = `ghapp — act as a GitHub App for git and gh

Usage:
  ghapp exec -- <cmd> [args...]   run cmd with the app's token and bot identity
                                  (e.g. ghapp exec -- git push; ghapp exec -- gh pr create)
  ghapp read <resource> <owner>/<repo> [number|ref]
                                  fetch PR, issue, or CI data; GET only, read-only token
  ghapp doctor [owner/repo]       list granted and missing app permissions
  ghapp setup-git [--undo]        make plain git in this repo commit/push as the app
  ghapp token [owner/repo]        print an installation token
  ghapp whoami                    print the app's bot name and email
  ghapp git-credential get        git credential helper (used by exec/setup-git)

Config: ~/.config/ghapp/config.json {"app_id": "...", "private_key_path": "..."}
        or GHAPP_APP_ID / GHAPP_PRIVATE_KEY_PATH (optional GHAPP_INSTALLATION_ID,
        GHAPP_API_URL, GHAPP_HOST, GHAPP_CONFIG).
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	c, err := newClient(cfg)
	if err != nil {
		return err
	}
	switch args[0] {
	case "token":
		owner, repo := originRepo(cfg.Host)
		if len(args) > 1 {
			owner, repo, _ = strings.Cut(args[1], "/")
		}
		t, err := c.installationToken(owner, repo)
		if err != nil {
			return err
		}
		fmt.Println(t.Token)
		return nil
	case "whoami":
		id, err := c.botIdentity()
		if err != nil {
			return err
		}
		fmt.Printf("%s <%s>\n", id.Name, id.Email)
		return nil
	case "git-credential":
		if len(args) < 2 || args[1] != "get" {
			return nil // store/erase: tokens are cached by ghapp itself
		}
		return credentialGet(c, os.Stdin, os.Stdout)
	case "exec":
		cmd := args[1:]
		if len(cmd) > 0 && cmd[0] == "--" {
			cmd = cmd[1:]
		}
		if len(cmd) == 0 {
			return errors.New("ghapp exec: missing command")
		}
		return execAs(c, cmd)
	case "read":
		return read(c, args[1:], os.Stdout)
	case "doctor":
		owner, repo := originRepo(cfg.Host)
		if len(args) > 1 {
			owner, repo, _ = strings.Cut(args[1], "/")
		}
		return doctor(c, owner, repo, os.Stdout)
	case "setup-git":
		return setupGit(c, len(args) > 1 && args[1] == "--undo")
	}
	return fmt.Errorf("ghapp: unknown command %q\n\n%s", args[0], usage)
}

// credentialGet implements the `get` action of git's credential helper protocol.
func credentialGet(c *client, in io.Reader, out io.Writer) error {
	attrs := map[string]string{}
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			break
		}
		attrs[k] = v
	}
	if attrs["host"] != c.cfg.Host {
		return nil // not ours; let git try other helpers or prompt
	}
	owner, repo := splitRepoPath(attrs["path"])
	if owner == "" {
		owner, repo = originRepo(c.cfg.Host)
	}
	t, err := c.installationToken(owner, repo)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "username=x-access-token\npassword=%s\n", t.Token)
	return err
}

// DECISION: hand the token to stock git/gh rather than commit via the API
// (see docs/decisions/0001-inject-app-token-into-stock-git-and-gh.md).
// gitSettings are the config entries that route git's GitHub traffic through the app.
// The empty helper value clears any earlier helpers (osxkeychain, gh auth git-credential)
// so the human's credentials can never be picked up.
func gitSettings(c *client) ([][2]string, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	base := "https://" + c.cfg.Host
	return [][2]string{
		{"credential." + base + ".helper", ""},
		{"credential." + base + ".helper", "!'" + strings.ReplaceAll(self, "'", `'\''`) + "' git-credential"},
		{"credential." + base + ".useHttpPath", "true"},
		{"url." + base + "/.insteadOf", "git@" + c.cfg.Host + ":"},
		{"url." + base + "/.insteadOf", "ssh://git@" + c.cfg.Host + "/"},
	}, nil
}

func execAs(c *client, argv []string) error {
	owner, repo := originRepo(c.cfg.Host)
	t, err := c.installationToken(owner, repo)
	if err != nil {
		return err
	}
	id, err := c.botIdentity()
	if err != nil {
		return err
	}
	settings, err := gitSettings(c)
	if err != nil {
		return err
	}
	env := os.Environ()
	n, _ := strconv.Atoi(os.Getenv("GIT_CONFIG_COUNT"))
	for _, kv := range settings {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n, kv[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n, kv[1]))
		n++
	}
	env = append(env,
		fmt.Sprintf("GIT_CONFIG_COUNT=%d", n),
		"GH_TOKEN="+t.Token,
		"GITHUB_TOKEN="+t.Token,
		"GIT_AUTHOR_NAME="+id.Name, "GIT_AUTHOR_EMAIL="+id.Email,
		"GIT_COMMITTER_NAME="+id.Name, "GIT_COMMITTER_EMAIL="+id.Email,
	)
	if c.cfg.Host != "github.com" {
		env = append(env, "GH_HOST="+c.cfg.Host, "GH_ENTERPRISE_TOKEN="+t.Token)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if fi, err := os.Stderr.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return cmd.Run() // a human's terminal: keep progress bars and prompts working
	}
	// Agents run without a terminal; watch stderr so permission failures get explained.
	tail := &tailWriter{}
	cmd.Stderr = io.MultiWriter(os.Stderr, tail)
	// Don't hang on background children (git maintenance) that inherit stderr.
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	if err != nil {
		if h := c.hint(argv, string(tail.b)); h != "" {
			fmt.Fprintln(os.Stderr, h)
		}
	}
	return err
}

// tailWriter keeps the last 64 KiB written to it.
type tailWriter struct{ b []byte }

func (w *tailWriter) Write(p []byte) (int, error) {
	const limit = 64 << 10
	w.b = append(w.b, p...)
	if len(w.b) > limit {
		w.b = append([]byte(nil), w.b[len(w.b)-limit:]...)
	}
	return len(p), nil
}

func setupGit(c *client, undo bool) error {
	settings, err := gitSettings(c)
	if err != nil {
		return err
	}
	keys := []string{"user.name", "user.email"}
	for _, kv := range settings {
		keys = append(keys, kv[0])
	}
	for _, k := range keys {
		// exit status 5 just means the key wasn't set
		_ = exec.Command("git", "config", "--local", "--unset-all", k).Run()
	}
	if undo {
		fmt.Println("ghapp: removed app git config from this repo")
		return nil
	}
	id, err := c.botIdentity()
	if err != nil {
		return err
	}
	settings = append(settings, [2]string{"user.name", id.Name}, [2]string{"user.email", id.Email})
	for _, kv := range settings {
		if out, err := exec.Command("git", "config", "--local", "--add", kv[0], kv[1]).CombinedOutput(); err != nil {
			return fmt.Errorf("git config %s: %v: %s", kv[0], err, out)
		}
	}
	fmt.Printf("ghapp: git in this repo now commits and pushes as %s <%s>\n", id.Name, id.Email)
	return nil
}

// originRepo returns owner/repo of the current directory's origin remote, if it is on host.
func originRepo(host string) (string, string) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", ""
	}
	return parseRemote(host, strings.TrimSpace(string(out)))
}

func parseRemote(host, remote string) (string, string) {
	for _, prefix := range []string{
		"git@" + host + ":",
		"ssh://git@" + host + "/",
		"https://" + host + "/",
		"http://" + host + "/",
	} {
		if rest, ok := strings.CutPrefix(remote, prefix); ok {
			return splitRepoPath(rest)
		}
	}
	// https://user@host/owner/repo
	if i := strings.Index(remote, "@"+host+"/"); i >= 0 && strings.HasPrefix(remote, "https://") {
		return splitRepoPath(remote[i+len(host)+2:])
	}
	return "", ""
}

func splitRepoPath(p string) (string, string) {
	owner, repo, ok := strings.Cut(strings.Trim(p, "/"), "/")
	if !ok {
		return "", ""
	}
	repo, _, _ = strings.Cut(repo, "/")
	return owner, strings.TrimSuffix(repo, ".git")
}

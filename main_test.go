package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func verifyJWT(t *testing.T, tok string, pub *rsa.PublicKey) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("bad signature: %v", err)
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(b, &claims)
	return claims
}

func TestAppJWT(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Unix(1_700_000_000, 0)
	tok, err := appJWT("123", key, now)
	if err != nil {
		t.Fatal(err)
	}
	c := verifyJWT(t, tok, &key.PublicKey)
	if c["iss"] != "123" || c["iat"].(float64) != 1_700_000_000-60 || c["exp"].(float64) != 1_700_000_000+540 {
		t.Fatalf("claims = %v", c)
	}
}

func TestParseRemote(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:DrHayt/ghapp.git":         "DrHayt/ghapp",
		"ssh://git@github.com/DrHayt/ghapp.git":   "DrHayt/ghapp",
		"https://github.com/DrHayt/ghapp":         "DrHayt/ghapp",
		"https://github.com/DrHayt/ghapp.git/":    "DrHayt/ghapp",
		"https://x-access-token@github.com/o/r":   "o/r",
		"https://gitlab.com/DrHayt/ghapp.git":     "/",
		"git@github.example.com:DrHayt/ghapp.git": "/",
	} {
		o, r := parseRemote("github.com", in)
		if got := o + "/" + r; got != want {
			t.Errorf("parseRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeGitHub serves the four endpoints ghapp uses and counts token mints.
func fakeGitHub(t *testing.T, pub *rsa.PublicKey, mints *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authJWT := func() bool {
			tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok {
				http.Error(w, "no auth", 401)
				return false
			}
			verifyJWT(t, tok, pub)
			return true
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/acme/widgets/installation":
			if authJWT() {
				w.Write([]byte(`{"id":42}`))
			}
		case r.Method == "POST" && r.URL.Path == "/app/installations/42/access_tokens":
			if authJWT() {
				mints.Add(1)
				exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
				w.Write([]byte(`{"token":"ghs_fake","expires_at":"` + exp + `"}`))
			}
		case r.Method == "GET" && r.URL.Path == "/app":
			if authJWT() {
				w.Write([]byte(`{"slug":"my-agent"}`))
			}
		case r.Method == "GET" && r.URL.Path == "/users/my-agent[bot]":
			w.Write([]byte(`{"id":99}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestEndToEnd builds the binary and drives it through real git, the way an agent would.
func TestEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "ghapp")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	keyPath := filepath.Join(tmp, "key.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	os.WriteFile(keyPath, pemBytes, 0o600)

	var mints atomic.Int32
	srv := fakeGitHub(t, &key.PublicKey, &mints)
	defer srv.Close()

	repo := filepath.Join(tmp, "repo")
	home := filepath.Join(tmp, "home")
	os.MkdirAll(home, 0o700)
	env := append(os.Environ(),
		"HOME="+home, "XDG_CACHE_HOME="+filepath.Join(home, "cache"),
		"GHAPP_CONFIG="+filepath.Join(home, "none.json"),
		"GHAPP_APP_ID=123", "GHAPP_PRIVATE_KEY_PATH="+keyPath, "GHAPP_API_URL="+srv.URL,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
	)
	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir, cmd.Env, cmd.Stdin = repo, env, strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	os.MkdirAll(repo, 0o700)
	run("", "git", "init", "-q")
	run("", "git", "remote", "add", "origin", "git@github.com:acme/widgets.git")
	// Simulate the human's own helper being configured; ghapp must override it.
	run("", "git", "config", "--global", "credential.helper", "!f() { echo username=human; echo password=HUMAN_TOKEN; }; f")

	t.Run("exec sets identity and token", func(t *testing.T) {
		got := run("", bin, "exec", "--", "sh", "-c", `echo "$GH_TOKEN|$GIT_AUTHOR_NAME|$GIT_COMMITTER_EMAIL"`)
		if want := "ghs_fake|my-agent[bot]|99+my-agent[bot]@users.noreply.github.com"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("exec rewrites ssh remote to https", func(t *testing.T) {
		if got := run("", bin, "exec", "--", "git", "remote", "get-url", "--push", "origin"); got != "https://github.com/acme/widgets.git" {
			t.Fatalf("push url = %q", got)
		}
	})
	t.Run("a global https-to-ssh rewrite cannot reach the human's key", func(t *testing.T) {
		const key = "url.git@github.com:.insteadOf"
		const pushKey = "url.git@github.com:.pushInsteadOf"
		run("", "git", "config", "--global", key, "https://github.com/")
		run("", "git", "config", "--global", pushKey, "https://github.com/")
		for _, remote := range []string{"https://github.com/acme/widgets.git", "git@github.com:acme/widgets.git", "ssh://git@github.com/acme/widgets.git"} {
			run("", "git", "remote", "set-url", "origin", remote)
			for _, dir := range []string{"--all", "--push"} {
				if got := run("", bin, "exec", "--", "git", "remote", "get-url", dir, "origin"); got != "https://github.com/acme/widgets.git" {
					t.Errorf("%s %s -> %q", remote, dir, got)
				}
			}
		}
		// Another owner is still rewritten to SSH: it must fail rather than connect.
		cmd := exec.Command(bin, "exec", "--", "git", "ls-remote", "https://github.com/other/repo.git")
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "'ssh' not allowed") {
			t.Errorf("ls-remote: %v: %s", err, out)
		}
		// setup-git must hold without exec's environment too.
		run("", bin, "setup-git")
		if got := run("", "git", "remote", "get-url", "--push", "origin"); got != "https://github.com/acme/widgets.git" {
			t.Errorf("setup-git push url = %q", got)
		}
		cmd = exec.Command("git", "ls-remote", "https://github.com/other/repo.git")
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "'ssh' not allowed") {
			t.Errorf("setup-git ls-remote: %v: %s", err, out)
		}
		run("", bin, "setup-git", "--undo")
		cmd = exec.Command("git", "config", "--local", "core.sshCommand")
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("undo left core.sshCommand = %s", out)
		}
		run("", "git", "config", "--global", "--unset", key)
		run("", "git", "config", "--global", "--unset", pushKey)
		run("", "git", "remote", "set-url", "origin", "git@github.com:acme/widgets.git")
	})
	t.Run("exec git uses app credential, not the human's", func(t *testing.T) {
		got := run("protocol=https\nhost=github.com\npath=acme/widgets.git\n\n", bin, "exec", "--", "git", "credential", "fill")
		if !strings.Contains(got, "username=x-access-token") || !strings.Contains(got, "password=ghs_fake") {
			t.Fatalf("credential fill = %q", got)
		}
	})
	t.Run("exec explains permission failures and keeps the exit code", func(t *testing.T) {
		gh := filepath.Join(tmp, "gh")
		os.WriteFile(gh, []byte("#!/bin/sh\necho 'Resource not accessible by integration' >&2; exit 7\n"), 0o700)
		cmd := exec.Command(bin, "exec", "--", gh, "run", "view", "1")
		cmd.Dir, cmd.Env = repo, env
		out, _ := cmd.CombinedOutput()
		if cmd.ProcessState.ExitCode() != 7 || !strings.Contains(string(out), "Actions: read") ||
			!strings.Contains(string(out), "/settings/apps/my-agent/permissions") {
			t.Fatalf("exit %d: %s", cmd.ProcessState.ExitCode(), out)
		}
	})
	t.Run("exec does not hang on background children", func(t *testing.T) {
		start := time.Now()
		cmd := exec.Command(bin, "exec", "--", "sh", "-c", "sleep 20 >/dev/null &")
		cmd.Dir, cmd.Env = repo, env
		// Own the outer pipe directly so only ghapp's own wait is measured.
		f, err := os.Create(filepath.Join(tmp, "stderr"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		cmd.Stderr = f
		if err := cmd.Run(); err != nil || time.Since(start) > 10*time.Second {
			t.Fatalf("err %v after %s", err, time.Since(start))
		}
	})
	t.Run("setup-git persists and commits as bot", func(t *testing.T) {
		run("", bin, "setup-git")
		got := run("protocol=https\nhost=github.com\npath=acme/widgets.git\n\n", "git", "credential", "fill")
		if !strings.Contains(got, "password=ghs_fake") {
			t.Fatalf("credential fill after setup-git = %q", got)
		}
		run("", "git", "commit", "-q", "--allow-empty", "-m", "test")
		if got := run("", "git", "log", "-1", "--format=%an <%ae>|%cn"); got != "my-agent[bot] <99+my-agent[bot]@users.noreply.github.com>|my-agent[bot]" {
			t.Fatalf("commit identity = %q", got)
		}
		run("", bin, "setup-git", "--undo")
		got = run("protocol=https\nhost=github.com\npath=acme/widgets.git\n\n", "git", "credential", "fill")
		if !strings.Contains(got, "HUMAN_TOKEN") {
			t.Fatalf("after undo expected the human helper back, got %q", got)
		}
	})
	localList := func() string {
		out := run("", "git", "config", "--local", "--get-regexp", `^(user|core\.sshcommand|protocol|credential|url)`)
		lines := strings.Split(out, "\n")
		// Order matters only among values of the same key.
		sort.SliceStable(lines, func(i, j int) bool {
			a, _, _ := strings.Cut(lines[i], " ")
			b, _, _ := strings.Cut(lines[j], " ")
			return a < b
		})
		for i := range lines { // run trims trailing space from the last line only
			lines[i] = strings.TrimRight(lines[i], " ")
		}
		return strings.Join(lines, "\n")
	}
	t.Run("inherited GIT_SSH_COMMAND cannot bypass the SSH block", func(t *testing.T) {
		run("", bin, "setup-git")
		defer run("", bin, "setup-git", "--undo")
		for _, argv := range [][]string{
			{"git", "ls-remote", "ssh://git@nohost.invalid/o/r"},
			{"git", "ls-remote", "git+ssh://git@nohost.invalid/o/r"},
			{bin, "exec", "--", "git", "ls-remote", "ssh://git@nohost.invalid/o/r"},
		} {
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Dir = repo
			cmd.Env = append(append([]string{}, env...), "GIT_SSH_COMMAND=echo HUMAN_SSH_RAN >&2; exit 1 #")
			if out, err := cmd.CombinedOutput(); err == nil || strings.Contains(string(out), "HUMAN_SSH_RAN") {
				t.Errorf("%v: %v: %s", argv, err, out)
			}
		}
	})
	t.Run("undo restores the exact prior config", func(t *testing.T) {
		run("", "git", "config", "user.name", "Human")
		run("", "git", "config", "user.email", "human@example.com")
		run("", "git", "config", "core.sshCommand", "ssh -i mine")
		run("", "git", "config", "url.https://github.com/.insteadOf", "gh:")
		run("", "git", "config", "credential.https://github.com.helper", "mine")
		list := func() string {
			l := strings.Split(run("", "git", "config", "--local", "--list"), "\n")
			sort.Strings(l)
			return strings.Join(l, "\n")
		}
		before := list()
		run("", bin, "setup-git")
		if got := run("", "git", "config", "--get-all", "url.https://github.com/.insteadOf"); !strings.Contains(got, "gh:") {
			t.Errorf("setup dropped the repo's own URL rule: %q", got)
		}
		run("", bin, "setup-git") // repeat must not record its own values as prior
		run("", "git", "remote", "set-url", "origin", "git@github.com:elsewhere/x.git")
		run("", bin, "setup-git", "--undo")
		run("", "git", "remote", "set-url", "origin", "git@github.com:acme/widgets.git")
		if after := list(); after != before {
			t.Errorf("before:\n%s\nafter:\n%s", before, after)
		}
		run("", bin, "setup-git", "--undo") // nothing recorded: must not touch anything
		if after := list(); after != before {
			t.Errorf("second undo changed config:\n%s", after)
		}
		// A value changed after setup belongs to the human.
		run("", bin, "setup-git")
		run("", "git", "config", "user.name", "Changed")
		run("", bin, "setup-git", "--undo")
		if got := run("", "git", "config", "user.name"); got != "Changed" {
			t.Errorf("undo overwrote a later change: %q", got)
		}
	})
	t.Run("undo of an unapplied record changes nothing", func(t *testing.T) {
		record := filepath.Join(repo, ".git", "ghapp-setup.json")
		run("", "git", "config", "--unset-all", "credential.https://github.com.helper")
		run("", "git", "config", "--add", "credential.https://github.com.helper", "")
		run("", "git", "config", "--add", "credential.https://github.com.helper", "mine")
		before := localList()
		run("", bin, "setup-git")
		b, err := os.ReadFile(record)
		if err != nil {
			t.Fatal(err)
		}
		run("", bin, "setup-git", "--undo")
		if err := os.WriteFile(record, b, 0o600); err != nil { // as if interrupted before applying
			t.Fatal(err)
		}
		run("", bin, "setup-git", "--undo")
		if after := localList(); after != before {
			t.Errorf("before:\n%s\nafter:\n%s", before, after)
		}
	})
	t.Run("concurrent setup and undo keep the original config", func(t *testing.T) {
		before := localList()
		var wg sync.WaitGroup
		for i := range 6 {
			wg.Go(func() {
				args := []string{"setup-git"}
				if i%3 == 2 {
					args = append(args, "--undo")
				}
				cmd := exec.Command(bin, args...)
				cmd.Dir, cmd.Env = repo, env
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("%v: %v: %s", args, err, out)
				}
			})
		}
		wg.Wait()
		run("", bin, "setup-git", "--undo")
		if after := localList(); after != before {
			t.Errorf("before:\n%s\nafter:\n%s", before, after)
		}
		if _, err := os.Stat(filepath.Join(repo, ".git", "ghapp-setup.json.lock")); err == nil {
			t.Error("lock left behind")
		}
	})
	t.Run("undo cleans up after versions without a record", func(t *testing.T) {
		run("", bin, "setup-git")
		os.Remove(filepath.Join(repo, ".git", "ghapp-setup.json"))
		run("", "git", "remote", "set-url", "origin", "git@github.com:elsewhere/x.git")
		if out := run("", bin, "setup-git", "--undo"); !strings.Contains(out, "--unset-all protocol.ssh.allow") {
			t.Errorf("leftover SSH block not reported: %s", out)
		}
		run("", "git", "config", "--unset-all", "protocol.ssh.allow")
		run("", "git", "remote", "set-url", "origin", "git@github.com:acme/widgets.git")
		got := run("", "git", "config", "--local", "--list")
		for _, bad := range []string{"acme/.", "git-credential", "[bot]", "ssh is disabled"} {
			if strings.Contains(strings.ToLower(got), bad) {
				t.Errorf("left %q in:\n%s", bad, got)
			}
		}
	})
	t.Run("tokens are cached", func(t *testing.T) {
		if n := mints.Load(); n != 1 {
			t.Fatalf("minted %d tokens, want 1", n)
		}
	})
}

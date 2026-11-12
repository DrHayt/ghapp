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
	"strings"
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
	t.Run("exec git uses app credential, not the human's", func(t *testing.T) {
		got := run("protocol=https\nhost=github.com\npath=acme/widgets.git\n\n", bin, "exec", "--", "git", "credential", "fill")
		if !strings.Contains(got, "username=x-access-token") || !strings.Contains(got, "password=ghs_fake") {
			t.Fatalf("credential fill = %q", got)
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
	t.Run("tokens are cached", func(t *testing.T) {
		if n := mints.Load(); n != 1 {
			t.Fatalf("minted %d tokens, want 1", n)
		}
	})
}

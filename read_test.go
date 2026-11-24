package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.HandlerFunc) *client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &client{cfg: &Config{AppID: "1", APIURL: srv.URL, Host: "github.com"}, key: key,
		http: srv.Client(), now: time.Now, cacheDir: t.TempDir()}
}

func TestReadIsReducedAndPaginates(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/acme/widgets/installation":
			fmt.Fprint(w, `{"id":42}`)
		case "POST /app/installations/42/access_tokens":
			var b struct {
				Repositories []string
				Permissions  map[string]string
			}
			json.NewDecoder(r.Body).Decode(&b)
			if fmt.Sprint(b.Repositories, b.Permissions) != "[widgets] map[checks:read]" {
				t.Errorf("token request not reduced: %+v", b)
			}
			fmt.Fprint(w, `{"token":"ghs_read"}`)
		case "GET /repos/acme/widgets/commits/feat/x/check-runs":
			if r.Header.Get("Authorization") != "Bearer ghs_read" {
				t.Error("wrong token")
			}
			n := 100
			if r.URL.Query().Get("page") == "2" {
				n = 1
			}
			fmt.Fprintf(w, `{"total_count":101,"check_runs":[%s]}`, strings.TrimSuffix(strings.Repeat(`{},`, n), ","))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	})
	var out bytes.Buffer
	if err := read(c, []string{"checks", "acme/widgets", "feat/x"}, &out); err != nil {
		t.Fatal(err)
	}
	var got []any
	if json.Unmarshal(out.Bytes(), &got); len(got) != 101 {
		t.Fatalf("got %d items", len(got))
	}
	if files, _ := os.ReadDir(c.cacheDir); len(files) != 0 {
		t.Fatal("read token was cached")
	}
}

func TestReadStatusPaginatesAndKeepsState(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/installation") {
			fmt.Fprint(w, `{"id":42}`)
			return
		}
		if r.Method == "POST" {
			fmt.Fprint(w, `{"token":"t"}`)
			return
		}
		if r.URL.Query().Get("per_page") != "100" {
			t.Error("not asking for full pages")
		}
		n := 100
		if r.URL.Query().Get("page") == "2" {
			n = 5
		}
		fmt.Fprintf(w, `{"state":"failure","total_count":105,"statuses":[%s]}`, strings.TrimSuffix(strings.Repeat(`{},`, n), ","))
	})
	var out bytes.Buffer
	if err := read(c, []string{"status", "a/b", "main"}, &out); err != nil {
		t.Fatal(err)
	}
	var got struct {
		State    string
		Statuses []any
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.State != "failure" || len(got.Statuses) != 105 {
		t.Fatalf("%v %s %d", err, got.State, len(got.Statuses))
	}
}

func TestDoctorReportsCacheFailure(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":42,"permissions":{"metadata":"read","contents":"write","pull_requests":"write","checks":"read","actions":"read"}}`)
	})
	if err := doctor(c, "a", "b", &bytes.Buffer{}); err != nil {
		t.Fatalf("no cache should be fine: %v", err)
	}
	// A non-empty directory cannot be removed.
	if err := os.MkdirAll(c.tokenCache("42")+"/x", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := doctor(c, "a", "b", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not cleared") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadRejectsBadArgs(t *testing.T) {
	for _, a := range []string{"", "nope a/b 1", "pr a/b", "pr a/b 1 2", "pr a/b 1/merge", "pr a/.. 1",
		"pr a/b/c 1", "pr a/b 1?x=y", "checks a/b ../../x", "checks a/b -x", "prs a/b --method=POST"} {
		if _, _, _, _, err := readPath(strings.Fields(a)); err == nil {
			t.Errorf("accepted %q", a)
		}
	}
	for n, r := range resources {
		if len(r.perm) != 1 {
			t.Errorf("%s: want one permission", n)
		}
		for _, level := range r.perm {
			if level != "read" {
				t.Errorf("%s asks for %s", n, level)
			}
		}
	}
}

func TestReadNamesMissingPermission(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			http.Error(w, `{"message":"not granted"}`, 422)
			return
		}
		fmt.Fprint(w, `{"id":42}`)
	})
	err := read(c, []string{"run", "a/b", "1"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `"actions" permission`) {
		t.Fatalf("err = %v", err)
	}
}

func TestDoctor(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app":
			fmt.Fprint(w, `{"slug":"bot","owner":{"login":"org","type":"Organization"},"permissions":{"checks":"read"}}`)
		case "/app/installations/42":
			fmt.Fprint(w, `{"html_url":"https://x/inst","permissions":{"metadata":"read","contents":"write","pull_requests":"write"}}`)
		default:
			fmt.Fprint(w, `{"id":42}`)
		}
	})
	writeCache(c.tokenCache("42"), token{Token: "old"})
	var out bytes.Buffer
	if doctor(c, "a", "b", &out) == nil {
		t.Error("want an error for missing required permissions")
	}
	for _, want := range []string{"ok         contents: write", "MISSING    checks: read", "awaiting acceptance",
		"MISSING    actions: read", "optional   workflows: write",
		"https://github.com/organizations/org/settings/apps/bot/permissions", "https://x/inst"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(c.tokenCache("42")); err == nil {
		t.Error("stale token kept")
	}
}

func TestPermissionHint(t *testing.T) {
	const denied = "gh: Resource not accessible by integration (HTTP 403)"
	for _, tc := range []struct{ cmd, stderr, want string }{
		{"gh pr checks 1", denied, "Checks: read"},
		{"gh run view 1", denied, "Actions: read"},
		{"git push", "! [remote rejected] refusing to allow a GitHub App to create or update workflow `.github/workflows/x.yml` without `workflows` permission", "Workflows: write"},
		{"git push", "fatal: unrelated", ""},
		{"gh run view 1 --repo acme/check-service", denied, "Actions: read"},
		{"gh pr create --title status", denied, "Pull requests: read to view"},
		{"gh api -X GET repos/acme/checks/pulls/1?x=check", denied, "Pull requests: read to view"},
		{"gh api /repos/a/b/commits/feat/x/check-runs", denied, "Checks: read"},
		{"gh api https://api.github.com/repos/a/b/actions/runs --jq .status", denied, "Actions: read"},
		{"gh api -X POST repos/a/b/check-runs", denied, "Checks: read to view, write"},
		{"gh api -X POST repos/a/b/statuses/abc", denied, "Commit statuses: read to view, write"},
		{"gh api repos/a/b/commits/main/status", denied, "Commit statuses: read"},
		{"gh api graphql -f query=checks", denied, "unknown"},
	} {
		got := permissionHint(strings.Fields(tc.cmd), tc.stderr)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q", tc.cmd, got)
		}
	}
}

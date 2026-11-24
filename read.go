package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// resource is one fixed GET endpoint that `ghapp read` can fetch.
type resource struct {
	path string            // below /repos/{owner}/{repo}/; {} is the argument
	arg  string            // "number", "ref", or "" for none
	list string            // "": single object; "[]": bare array; else the array's key
	perm map[string]string // the only permissions the token is minted with
}

func readPerm(name string) map[string]string { return map[string]string{name: "read"} }

// DECISION: a closed table of GETs, each with its own read-only token, so that
// `ghapp read` is safe to allow by prefix. Do not add a generic path/method
// (see docs/decisions/0002-read-command-with-reduced-tokens.md).
var resources = map[string]resource{
	"pr":              {"pulls/{}", "number", "", readPerm("pull_requests")},
	"prs":             {"pulls", "", "[]", readPerm("pull_requests")},
	"pr-reviews":      {"pulls/{}/reviews", "number", "[]", readPerm("pull_requests")},
	"pr-comments":     {"pulls/{}/comments", "number", "[]", readPerm("pull_requests")},
	"pr-conversation": {"issues/{}/comments", "number", "[]", readPerm("pull_requests")},
	"pr-files":        {"pulls/{}/files", "number", "[]", readPerm("pull_requests")},
	"pr-commits":      {"pulls/{}/commits", "number", "[]", readPerm("pull_requests")},
	"issue":           {"issues/{}", "number", "", readPerm("issues")},
	"issue-comments":  {"issues/{}/comments", "number", "[]", readPerm("issues")},
	"checks":          {"commits/{}/check-runs", "ref", "check_runs", readPerm("checks")},
	"status":          {"commits/{}/status", "ref", "", readPerm("statuses")},
	"run":             {"actions/runs/{}", "number", "", readPerm("actions")},
	"run-jobs":        {"actions/runs/{}/jobs", "number", "jobs", readPerm("actions")},
	"job-log":         {"actions/jobs/{}/logs", "number", "raw", readPerm("actions")},
}

var (
	nameRE   = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	numberRE = regexp.MustCompile(`^[0-9]+$`)
	refRE    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./-]*$`)
)

func readUsage() string {
	var b strings.Builder
	b.WriteString("usage: ghapp read <resource> <owner>/<repo> [number|ref]\n\nresources:\n")
	names := make([]string, 0, len(resources))
	for n := range resources {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "  %-16s %s\n", n, resources[n].arg)
	}
	return b.String()
}

// readPath validates args and returns the API path for res.
func readPath(args []string) (res resource, owner, repo, path string, err error) {
	fail := func(msg string) (resource, string, string, string, error) {
		return resource{}, "", "", "", fmt.Errorf("ghapp read: %s\n\n%s", msg, readUsage())
	}
	if len(args) < 2 {
		return fail("missing arguments")
	}
	res, ok := resources[args[0]]
	if !ok {
		return fail("unknown resource " + args[0])
	}
	owner, repo, _ = strings.Cut(args[1], "/")
	for _, s := range []string{owner, repo} {
		if !nameRE.MatchString(s) || s == "." || s == ".." {
			return fail("expected owner/repo, got " + args[1])
		}
	}
	want := 2
	if res.arg != "" {
		want = 3
	}
	if len(args) != want {
		return fail("wrong number of arguments for " + args[0])
	}
	sub := res.path
	if res.arg != "" {
		re := numberRE
		if res.arg == "ref" {
			re = refRE
		}
		if !re.MatchString(args[2]) || strings.Contains(args[2], "..") {
			return fail("invalid " + res.arg + " " + args[2])
		}
		sub = strings.Replace(sub, "{}", url.PathEscape(args[2]), 1)
	}
	return res, owner, repo, "/repos/" + owner + "/" + repo + "/" + sub, nil
}

func read(c *client, args []string, out io.Writer) error {
	res, owner, repo, path, err := readPath(args)
	if err != nil {
		return err
	}
	t, err := c.readToken(owner, repo, res.perm)
	if err != nil {
		return err
	}
	if res.list == "raw" {
		var raw []byte
		if err := c.do("GET", path, t.Token, nil, &raw); err != nil {
			return err
		}
		_, err := out.Write(raw)
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if res.list == "" {
		var v json.RawMessage
		if err := c.do("GET", path, t.Token, nil, &v); err != nil {
			return err
		}
		return enc.Encode(v)
	}
	all := []json.RawMessage{}
	for page := 1; ; page++ {
		var items []json.RawMessage
		var dst any = &items
		obj := map[string]json.RawMessage{}
		if res.list != "[]" {
			dst = &obj
		}
		if err := c.do("GET", fmt.Sprintf("%s?per_page=100&page=%d", path, page), t.Token, nil, dst); err != nil {
			return err
		}
		if res.list != "[]" {
			if err := json.Unmarshal(obj[res.list], &items); err != nil {
				return fmt.Errorf("unexpected response from %s: %w", path, err)
			}
		}
		all = append(all, items...)
		if len(items) < 100 {
			return enc.Encode(all)
		}
	}
}

// readToken mints a token limited to one repo and perm. It is deliberately not
// cached, so it can never be mixed up with the full token that exec uses.
func (c *client) readToken(owner, repo string, perm map[string]string) (*token, error) {
	id, err := c.installationID(owner, repo)
	if err != nil {
		return nil, err
	}
	j, err := c.jwt()
	if err != nil {
		return nil, err
	}
	var t token
	body := map[string]any{"repositories": []string{repo}, "permissions": perm}
	if err := c.do("POST", "/app/installations/"+id+"/access_tokens", j, body, &t); err != nil {
		if strings.Contains(err.Error(), ": 422 ") {
			for p, level := range perm {
				return nil, fmt.Errorf("%w\nghapp: this needs the %q permission (%s), and the app must have access to %s/%s; run `ghapp doctor %s/%s`",
					err, p, level, owner, repo, owner, repo)
			}
		}
		return nil, err
	}
	return &t, nil
}

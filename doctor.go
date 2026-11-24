package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

type need struct {
	perm, level string
	required    bool
	breaks      string
}

// needs is the documented permission set; keep in sync with README.
var needs = []need{
	{"metadata", "read", true, "everything"},
	{"contents", "write", true, "pushing branches"},
	{"pull_requests", "write", true, "opening and reading pull requests"},
	{"checks", "read", true, "reading CI check results"},
	{"actions", "read", true, "reading Actions runs and job logs"},
	{"statuses", "read", false, "reading commit statuses (gh pr checks, some CI systems)"},
	{"issues", "read", false, "reading issues"},
	{"workflows", "write", false, "pushing changes to .github/workflows (leaving this off is a reasonable safeguard)"},
}

var rank = map[string]int{"": 0, "read": 1, "write": 2, "admin": 3}

type appInfo struct {
	Slug        string            `json:"slug"`
	Permissions map[string]string `json:"permissions"`
	Owner       struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"owner"`
}

func (a appInfo) settingsURL(host string) string {
	scope := ""
	if a.Owner.Type == "Organization" {
		scope = "/organizations/" + a.Owner.Login
	}
	return "https://" + host + scope + "/settings/apps/" + a.Slug + "/permissions"
}

func doctor(c *client, owner, repo string, out io.Writer) error {
	j, err := c.jwt()
	if err != nil {
		return err
	}
	var app appInfo
	if err := c.do("GET", "/app", j, nil, &app); err != nil {
		return err
	}
	id, err := c.installationID(owner, repo)
	if err != nil {
		return err
	}
	var inst struct {
		Permissions map[string]string `json:"permissions"`
		HTMLURL     string            `json:"html_url"`
		SuspendedAt *string           `json:"suspended_at"`
	}
	if err := c.do("GET", "/app/installations/"+id, j, nil, &inst); err != nil {
		return err
	}
	fmt.Fprintf(out, "app %s, installation %s\n", app.Slug, id)
	bad := false
	if inst.SuspendedAt != nil {
		bad = true
		fmt.Fprintln(out, "SUSPENDED  the installation is suspended; nothing will work")
	}
	var grant, accept bool
	for _, n := range needs {
		if rank[inst.Permissions[n.perm]] >= rank[n.level] {
			fmt.Fprintf(out, "ok         %s: %s\n", n.perm, inst.Permissions[n.perm])
			continue
		}
		label := "optional  "
		if n.required {
			label, bad = "MISSING   ", true
		}
		note := ""
		if rank[app.Permissions[n.perm]] >= rank[n.level] {
			note = " [set on the app, awaiting acceptance by the installation]"
			accept = true
		} else {
			grant = true
		}
		fmt.Fprintf(out, "%s %s: %s; without it, this fails: %s%s\n", label, n.perm, n.level, n.breaks, note)
	}
	if grant {
		fmt.Fprintf(out, "\nAdd permissions: %s\n", app.settingsURL(c.cfg.Host))
	}
	if grant || accept {
		fmt.Fprintf(out, "Then accept them for the installation: %s\n", inst.HTMLURL)
	}
	// A cached token keeps the permissions it was minted with.
	if err := os.Remove(c.tokenCache(id)); err == nil {
		fmt.Fprintln(out, "Cleared the cached token, so permission changes apply to the next command.")
	}
	if bad {
		return errors.New("ghapp doctor: problems found")
	}
	return nil
}

// permissionHint names the permission a failed command probably lacked, or "".
func permissionHint(argv []string, stderr string) string {
	low := strings.ToLower(stderr)
	perm := ""
	switch {
	case strings.Contains(low, "refusing to allow a github app") && strings.Contains(low, "workflow"):
		perm = "Workflows: write"
	case strings.Contains(low, "resource not accessible by integration"):
		cmd := strings.ToLower(strings.Join(argv, " "))
		switch {
		case strings.Contains(cmd, "check") || strings.Contains(cmd, "status"):
			perm = "Checks: read (and Commit statuses: read)"
		case strings.Contains(cmd, "/actions/") || strings.Contains(cmd, "gh run ") || strings.Contains(cmd, "gh workflow "):
			perm = "Actions: read (write to re-run or cancel)"
		case strings.Contains(cmd, "gh pr ") || strings.Contains(cmd, "/pulls"):
			perm = "Pull requests: write"
		case strings.Contains(cmd, "gh issue ") || strings.Contains(cmd, "/issues"):
			perm = "Issues: write (or read, to view)"
		default:
			perm = "unknown"
		}
	default:
		return ""
	}
	return "ghapp: the app probably lacks a permission: " + perm
}

// hint adds the place to grant it. Best effort: it never masks the real failure.
func (c *client) hint(argv []string, stderr string) string {
	h := permissionHint(argv, stderr)
	if h == "" {
		return ""
	}
	if j, err := c.jwt(); err == nil {
		var app appInfo
		if c.do("GET", "/app", j, nil, &app) == nil {
			h += "\nghapp: grant it at " + app.settingsURL(c.cfg.Host) + " and accept it on the installation"
		}
	}
	return h + "\nghapp: then run `ghapp doctor` to verify"
}

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// setupState records what setup-git changed, so undo doesn't depend on the
// current origin, executable path, or app config.
type setupState struct {
	Added    [][2]string         `json:"added"`
	Previous map[string][]string `json:"previous"` // local values that were replaced
}

func setupStatePath() (string, error) {
	// --local config is shared by all worktrees, so the record must be too.
	out, err := exec.Command("git", "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return "", errors.New("ghapp setup-git: not inside a git repository")
	}
	return filepath.Join(strings.TrimSpace(string(out)), "ghapp-setup.json"), nil
}

// localConfig runs `git config --local`; "no match" (1 or 5) is not an error.
func localConfig(args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"config", "--local"}, args...)...).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code == 1 || code == 5 {
			return "", nil
		}
		return "", fmt.Errorf("git config %s: %w: %s", strings.Join(args, " "), err, ee.Stderr)
	}
	return string(out), err
}

func localValues(key string) ([]string, error) {
	out, err := localConfig("-z", "--get-all", key)
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00"), nil
}

// lockSetup serializes setup and undo across all worktrees of a repo.
func lockSetup(statePath string) (unlock func(), err error) {
	path := statePath + ".lock"
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("ghapp setup-git: another setup or undo is running; if not, remove %s", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func setupGit(c *client, undo bool) error {
	path, err := setupStatePath()
	if err != nil {
		return err
	}
	if undo {
		unlock, err := lockSetup(path)
		if err != nil {
			return err
		}
		defer unlock()
		if err := undoSetup(c, path); err != nil {
			return err
		}
		fmt.Println("ghapp: removed app git config from this repo")
		return nil
	}
	// Resolve everything that can fail before touching the repo.
	id, err := c.botIdentity()
	if err != nil {
		return err
	}
	unlock, err := lockSetup(path)
	if err != nil {
		return err
	}
	defer unlock()
	if err := undoSetup(c, path); err != nil {
		return err
	}
	settings, err := gitSettings(c)
	if err != nil {
		return err
	}
	settings = append(settings, [2]string{"user.name", id.Name}, [2]string{"user.email", id.Email})

	st := setupState{Previous: map[string][]string{}}
	var replace []string
	for _, kv := range settings {
		vals, err := localValues(kv[0])
		if err != nil {
			return err
		}
		if strings.HasPrefix(kv[0], "url.") {
			if !slices.Contains(vals, kv[1]) { // keep the repo's own rules
				st.Added = append(st.Added, kv)
			}
			continue
		}
		st.Added = append(st.Added, kv)
		if !slices.Contains(replace, kv[0]) {
			replace = append(replace, kv[0])
			if vals != nil {
				st.Previous[kv[0]] = vals
			}
		}
	}
	// Write the record first, so an interrupted setup can still be undone.
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	// Rename, so an interrupted write can't leave an unreadable record.
	if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	for _, k := range replace {
		if _, err := localConfig("--unset-all", k); err != nil {
			return err
		}
	}
	for _, kv := range st.Added {
		if _, err := localConfig("--add", kv[0], kv[1]); err != nil {
			return err
		}
	}
	fmt.Printf("ghapp: git in this repo now commits and pushes as %s <%s>\n", id.Name, id.Email)
	return nil
}

// undoSetup reverses a recorded setup. Values changed since then are left alone.
func undoSetup(c *client, path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return undoUnrecorded(c)
	}
	if err != nil {
		return err
	}
	var st setupState
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	// If a key holds a leading part of its prior values, then setup hadn't replaced
	// it yet or an earlier undo was interrupted while restoring it. Only add the rest:
	// removing "ours" could drop a prior value that happens to be equal.
	restoring := map[string]bool{}
	for k, prev := range st.Previous {
		cur, err := localValues(k)
		if err != nil {
			return err
		}
		if len(cur) > len(prev) || !slices.Equal(cur, prev[:len(cur)]) {
			continue
		}
		restoring[k] = true
		for _, v := range prev[len(cur):] {
			if _, err := localConfig("--add", k, v); err != nil {
				return err
			}
		}
	}
	for _, kv := range st.Added {
		if restoring[kv[0]] {
			continue
		}
		if _, err := localConfig("--fixed-value", "--unset-all", kv[0], kv[1]); err != nil {
			return err
		}
	}
	for k, prev := range st.Previous {
		if restoring[k] {
			continue
		}
		left, err := localValues(k)
		if err != nil {
			return err
		}
		if left != nil {
			continue
		}
		for _, v := range prev {
			if _, err := localConfig("--add", k, v); err != nil {
				return err
			}
		}
	}
	return os.Remove(path)
}

// undoUnrecorded cleans up after versions of ghapp that kept no record.
func undoUnrecorded(c *client) error {
	base := "https://" + c.cfg.Host
	helperKey := "credential." + base + ".helper"
	vals, err := localValues(helperKey)
	if err != nil {
		return err
	}
	ours := func(v string) bool {
		return strings.HasPrefix(v, "!'") && strings.HasSuffix(v, "ghapp' git-credential")
	}
	if !slices.ContainsFunc(vals, ours) {
		return nil
	}
	for _, k := range []string{helperKey, "credential." + base + ".useHttpPath"} {
		if _, err := localConfig("--unset-all", k); err != nil {
			return err
		}
	}
	if _, err := localConfig("--fixed-value", "--unset-all", "core.sshCommand", sshBlocked); err != nil {
		return err
	}
	// Nothing proves ghapp set this one, and removing a human's own block would
	// weaken their config, so report it instead.
	if v, err := localValues("protocol.ssh.allow"); err != nil {
		return err
	} else if slices.Contains(v, "never") {
		fmt.Fprintln(os.Stderr, "ghapp: no setup record found, so protocol.ssh.allow=never was left in place and SSH stays blocked."+
			"\nghapp: if you didn't set it, run: git config --local --unset-all protocol.ssh.allow")
	}
	name, err := localValues("user.name")
	if err != nil {
		return err
	}
	if len(name) == 1 && strings.HasSuffix(name[0], "[bot]") {
		for _, k := range []string{"user.name", "user.email"} {
			if _, err := localConfig("--unset-all", k); err != nil {
				return err
			}
		}
	}
	// URL rules for any owner: the origin may have changed since setup.
	out, err := localConfig("-z", "--get-regexp", `^url\.https://.*\.(push)?insteadof$`)
	if err != nil {
		return err
	}
	for _, entry := range strings.Split(out, "\x00") {
		key, val, _ := strings.Cut(entry, "\n")
		target := strings.TrimPrefix(key[:max(strings.LastIndex(key, "."), 0)], "url.")
		rest, ok := strings.CutPrefix(target, base+"/")
		if !ok {
			continue
		}
		if slices.Contains([]string{"git@" + c.cfg.Host + ":" + rest, "ssh://git@" + c.cfg.Host + "/" + rest, target}, val) {
			if _, err := localConfig("--fixed-value", "--unset-all", key, val); err != nil {
				return err
			}
		}
	}
	return nil
}

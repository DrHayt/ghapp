package main

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config identifies the GitHub App. Values come from ~/.config/ghapp/config.json,
// overridden by GHAPP_* environment variables.
type Config struct {
	AppID          string `json:"app_id"`           // numeric App ID or Client ID
	PrivateKeyPath string `json:"private_key_path"` // PEM downloaded from the App settings page
	InstallationID string `json:"installation_id"`  // optional; otherwise looked up per repo
	APIURL         string `json:"api_url"`          // optional; for GitHub Enterprise Server
	Host           string `json:"host"`             // optional git host, default github.com
}

func configPath() string {
	if p := os.Getenv("GHAPP_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "ghapp", "config.json")
}

func loadConfig() (*Config, error) {
	c := &Config{}
	path := configPath()
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	for env, dst := range map[string]*string{
		"GHAPP_APP_ID":           &c.AppID,
		"GHAPP_PRIVATE_KEY_PATH": &c.PrivateKeyPath,
		"GHAPP_INSTALLATION_ID":  &c.InstallationID,
		"GHAPP_API_URL":          &c.APIURL,
		"GHAPP_HOST":             &c.Host,
	} {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	if c.APIURL == "" {
		c.APIURL = "https://api.github.com"
	}
	c.APIURL = strings.TrimRight(c.APIURL, "/")
	if c.Host == "" {
		c.Host = "github.com"
	}
	if c.AppID == "" || c.PrivateKeyPath == "" {
		return nil, fmt.Errorf("ghapp: app_id and private_key_path must be set in %s or via GHAPP_APP_ID / GHAPP_PRIVATE_KEY_PATH", path)
	}
	if strings.HasPrefix(c.PrivateKeyPath, "~/") {
		home, _ := os.UserHomeDir()
		c.PrivateKeyPath = filepath.Join(home, c.PrivateKeyPath[2:])
	}
	return c, nil
}

func loadKey(path string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an RSA key", path)
	}
	return rk, nil
}

// appJWT builds the short-lived RS256 JWT GitHub requires to authenticate as the App.
func appJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(), // allow for clock drift
		"exp": now.Add(9 * time.Minute).Unix(),   // GitHub max is 10 minutes
		"iss": appID,
	})
	signingInput := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + enc.EncodeToString(sig), nil
}

type client struct {
	cfg      *Config
	key      *rsa.PrivateKey
	http     *http.Client
	now      func() time.Time
	cacheDir string
}

func newClient(cfg *Config) (*client, error) {
	key, err := loadKey(cfg.PrivateKeyPath)
	if err != nil {
		return nil, err
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return &client{
		cfg: cfg, key: key, now: time.Now,
		http:     &http.Client{Timeout: 30 * time.Second},
		cacheDir: filepath.Join(dir, "ghapp"),
	}, nil
}

func (c *client) do(method, path, bearer string, out any) error {
	req, err := http.NewRequest(method, c.cfg.APIURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(bytes.NewReader(b)).Decode(out)
	}
	return nil
}

func (c *client) jwt() (string, error) { return appJWT(c.cfg.AppID, c.key, c.now()) }

// installationID returns the configured installation, or the one covering owner/repo.
func (c *client) installationID(owner, repo string) (string, error) {
	if c.cfg.InstallationID != "" {
		return c.cfg.InstallationID, nil
	}
	if owner == "" || repo == "" {
		return "", errors.New("ghapp: cannot determine repository; run inside a clone with a GitHub origin or set GHAPP_INSTALLATION_ID")
	}
	j, err := c.jwt()
	if err != nil {
		return "", err
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/installation"
	if err := c.do("GET", path, j, &inst); err != nil {
		return "", fmt.Errorf("is the app installed on %s/%s? %w", owner, repo, err)
	}
	return strconv.FormatInt(inst.ID, 10), nil
}

type token struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// installationToken returns a cached token if it has >5 minutes left, else mints one.
func (c *client) installationToken(owner, repo string) (*token, error) {
	id, err := c.installationID(owner, repo)
	if err != nil {
		return nil, err
	}
	cachePath := filepath.Join(c.cacheDir, "token-"+c.cfg.AppID+"-"+id+".json")
	var t token
	if readCache(cachePath, &t) && t.ExpiresAt.After(c.now().Add(5*time.Minute)) {
		return &t, nil
	}
	j, err := c.jwt()
	if err != nil {
		return nil, err
	}
	if err := c.do("POST", "/app/installations/"+id+"/access_tokens", j, &t); err != nil {
		return nil, err
	}
	writeCache(cachePath, t)
	return &t, nil
}

type identity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// botIdentity returns the "<slug>[bot]" name and noreply email GitHub attributes to the App.
func (c *client) botIdentity() (*identity, error) {
	cachePath := filepath.Join(c.cacheDir, "identity-"+c.cfg.AppID+".json")
	var id identity
	if readCache(cachePath, &id) && id.Name != "" {
		return &id, nil
	}
	j, err := c.jwt()
	if err != nil {
		return nil, err
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := c.do("GET", "/app", j, &app); err != nil {
		return nil, err
	}
	login := app.Slug + "[bot]"
	var user struct {
		ID int64 `json:"id"`
	}
	if err := c.do("GET", "/users/"+url.PathEscape(login), "", &user); err != nil {
		return nil, err
	}
	id = identity{Name: login, Email: fmt.Sprintf("%d+%s@users.noreply.%s", user.ID, login, c.cfg.Host)}
	writeCache(cachePath, id)
	return &id, nil
}

func readCache(path string, v any) bool {
	b, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(b, v) == nil
}

// writeCache is best-effort: a failed write only costs an extra API call next time.
func writeCache(path string, v any) {
	b, _ := json.Marshal(v)
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

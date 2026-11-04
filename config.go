package main

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration. Everything is optional and has a
// conservative default so that the service is safe to expose on a private
// network without extra configuration.
type Config struct {
	StorePath  string
	ListenAddr string

	// AllowedRemotes is the exact set of hosts that may appear as the first
	// path segment (for example "github.com"). It must not be derived from
	// user input.
	AllowedRemotes map[string]bool

	// AllowedReleaseHosts is the exact set of hosts (optionally host:port)
	// that /releases/ may fetch from, including redirect targets.
	AllowedReleaseHosts map[string]bool

	RepoTTL      time.Duration // how long a cached repository is considered fresh
	CloneTimeout time.Duration // upper bound for a git clone/fetch operation
	HTTPTimeout  time.Duration // upper bound for a HEAD request and response headers

	DownloadTimeout     time.Duration // upper bound for a full release download
	MaxDownloadSize     int64         // 0 means unlimited
	DownloadConcurrency int
	CloneConcurrency    int

	ReleaseCacheTTL      time.Duration // 0 disables age-based eviction
	ReleaseCacheMaxBytes int64         // 0 disables size-based eviction

	// ObjectReuse lets a new repository clone borrow objects from an already
	// cached repository with the same repository name (forks, or repo vs
	// repo.git). It is safe for a cache that only contains public repositories;
	// disable it if private repositories share the store.
	ObjectReuse bool

	// FetchPullRefs caches GitHub pull request refs (refs/pull/*/head) so that
	// clients can fetch them with the normal repository URL. PullRefsTTL limits
	// how often that (potentially large) ref set is refreshed.
	FetchPullRefs bool
	PullRefsTTL   time.Duration

	GitPath string // optional explicit git binary
}

func loadConfig() Config {
	cfg := Config{
		StorePath:  envString("STORE_PATH", "data"),
		ListenAddr: envString("LISTEN_ADDR", ":8080"),

		AllowedRemotes:      parseHostSet(envString("ALLOWED_REMOTES", "github.com")),
		AllowedReleaseHosts: parseHostSet(envString("ALLOWED_RELEASE_HOSTS", "github.com,objects.githubusercontent.com,release-assets.githubusercontent.com,github-releases.githubusercontent.com,codeload.github.com")),

		RepoTTL:      envDuration("REPO_TTL", 0),
		CloneTimeout: envDuration("CLONE_TIMEOUT", 10*time.Minute),
		HTTPTimeout:  envDuration("HTTP_TIMEOUT", 30*time.Second),

		DownloadTimeout:     envDuration("DOWNLOAD_TIMEOUT", 30*time.Minute),
		MaxDownloadSize:     envInt64("MAX_DOWNLOAD_SIZE", 2<<30),
		DownloadConcurrency: envInt("DOWNLOAD_CONCURRENCY", 4),
		CloneConcurrency:    envInt("CLONE_CONCURRENCY", 4),

		ReleaseCacheTTL:      envDuration("RELEASE_CACHE_TTL", 0),
		ReleaseCacheMaxBytes: envInt64("RELEASE_CACHE_MAX_BYTES", 0),

		ObjectReuse:   envBool("OBJECT_REUSE", true),
		FetchPullRefs: envBool("FETCH_PULL_REFS", false),
		PullRefsTTL:   envDuration("PULL_REFS_TTL", 0),

		GitPath: envString("GIT_PATH", ""),
	}
	if cfg.StorePath == "" {
		cfg.StorePath = "data"
	}
	return cfg
}

func envString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	raw := envString(key, "")
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		log.Printf("invalid %s=%q, using %s", key, raw, def)
		return def
	}
	return d
}

func envInt64(key string, def int64) int64 {
	raw := envString(key, "")
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		log.Printf("invalid %s=%q, using %d", key, raw, def)
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	raw := envString(key, "")
	if raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		log.Printf("invalid %s=%q, using %t", key, raw, def)
		return def
	}
	return v
}

func envInt(key string, def int) int {
	raw := envString(key, "")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		log.Printf("invalid %s=%q, using %d", key, raw, def)
		return def
	}
	return n
}

// parseHostSet parses a comma separated allowlist. Entries are matched
// case-insensitively; host:port entries are allowed for non-standard ports.
func parseHostSet(raw string) map[string]bool {
	set := make(map[string]bool)
	for _, part := range strings.Split(raw, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		set[part] = true
	}
	return set
}

func (c Config) allowedRemote(remote string) bool {
	remote = strings.ToLower(strings.TrimSpace(remote))
	if remote == "" || remote == "." || remote == ".." {
		return false
	}
	if strings.ContainsAny(remote, `/\`) {
		return false
	}
	return c.AllowedRemotes[remote]
}

// allowedReleaseURL validates scheme, credentials and host. Redirect targets
// go through the same check in the HTTP client's CheckRedirect.
func (c Config) allowedReleaseURL(u *url.URL) bool {
	if u == nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}

	// Allow an explicit host:port entry first.
	if c.AllowedReleaseHosts[strings.ToLower(u.Host)] {
		return true
	}

	// Then allow the hostname for the scheme's default port.
	port := u.Port()
	if port == "" || (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		return c.AllowedReleaseHosts[host]
	}
	return false
}

func (c Config) parseReleaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if !u.IsAbs() || u.Host == "" {
		return nil, fmt.Errorf("url must be absolute")
	}
	if u.Fragment != "" {
		return nil, fmt.Errorf("url fragments are not supported")
	}
	if !c.allowedReleaseURL(u) {
		return nil, fmt.Errorf("host %q is not allowed", u.Host)
	}
	return u, nil
}

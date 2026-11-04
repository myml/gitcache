package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

var (
	gitBinary     string
	repoGroup     singleflight.Group
	repoLocks     sync.Map // repo path -> *sync.RWMutex
	repoUpdateSem chan struct{}
)

var segmentRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validGitAction is an exact allowlist. Keeping this list small means
// http-backend never receives a user-controlled path containing "..", even
// though we also validate every path segment separately.
func validGitAction(action string) bool {
	switch action {
	case "/info/refs", "/git-upload-pack", "/git-receive-pack":
		return true
	default:
		return false
	}
}

func validateSegment(kind, value string) error {
	if value == "" || value == "." || value == ".." {
		return fmt.Errorf("invalid %s", kind)
	}
	if len(value) > 255 {
		return fmt.Errorf("%s is too long", kind)
	}
	if !segmentRE.MatchString(value) {
		return fmt.Errorf("invalid characters in %s", kind)
	}
	return nil
}

func normalizeOwnerRepo(owner, repo string) (string, string, error) {
	if err := validateSegment("owner", owner); err != nil {
		return "", "", err
	}
	// GitHub and GitLab both accept the same repository with or without a
	// trailing .git. Normalizing avoids two cache directories for one repo.
	repo = strings.TrimSuffix(repo, ".git")
	if err := validateSegment("repo", repo); err != nil {
		return "", "", err
	}
	return owner, repo, nil
}

// repoStorePath returns the absolute cache path and guarantees it stays below
// the configured store directory.
func repoStorePath(store, remote, owner, repo string) (string, error) {
	absStore, err := filepath.Abs(store)
	if err != nil {
		return "", err
	}
	p := filepath.Join(absStore, remote, owner, repo)
	rel, err := filepath.Rel(absStore, p)
	if err != nil {
		return "", err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("repository path escapes store")
	}
	return p, nil
}

func repoURL(remote, owner, repo string) string {
	u := url.URL{
		Scheme: "https",
		Host:   remote,
		Path:   "/" + owner + "/" + repo,
	}
	return u.String()
}

func repoLock(key string) *sync.RWMutex {
	v, _ := repoLocks.LoadOrStore(key, &sync.RWMutex{})
	return v.(*sync.RWMutex)
}

func repoExists(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, "HEAD"))
	return err == nil && !fi.IsDir()
}

func repoFresh(dir string, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, ".last-update"))
	return err == nil && time.Since(fi.ModTime()) < ttl
}

func touchRepo(dir string) error {
	return os.WriteFile(filepath.Join(dir, ".last-update"), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
}

// ensureRepo refreshes a cached repository. A failed refresh of an existing
// repository is not fatal: stale data is better than no data for a cache.
func ensureRepo(ctx context.Context, cfg Config, remote, owner, repo, dir string, force bool) error {
	if !force && repoFresh(dir, cfg.RepoTTL) {
		return nil
	}

	lock := repoLock(dir)
	_, err, _ := repoGroup.Do(dir, func() (interface{}, error) {
		lock.Lock()
		defer lock.Unlock()

		if !force && repoFresh(dir, cfg.RepoTTL) {
			return nil, nil
		}

		opCtx, cancel := context.WithTimeout(ctx, cfg.CloneTimeout)
		defer cancel()

		if repoExists(dir) {
			if err := withUpdateSlot(opCtx, func() error {
				return updateRepo(opCtx, cfg, dir)
			}); err != nil {
				if repoExists(dir) {
					log.Printf("refresh %s failed, keeping stale copy: %v", dir, err)
					return nil, nil
				}
				return nil, err
			}
		} else {
			ref := ""
			if cfg.ObjectReuse {
				ref = findReferenceRepo(cfg.StorePath, remote, repo, dir)
			}
			if ref != "" {
				// Keep the reference stable while git reads its objects. The
				// lock is taken before the update semaphore so that a reference
				// refresh cannot hold the semaphore while waiting for this lock.
				refLock := repoLock(ref)
				refLock.RLock()
				defer refLock.RUnlock()
				if !repoExists(ref) {
					ref = ""
				}
			}
			if err := withUpdateSlot(opCtx, func() error {
				return cloneRepo(opCtx, cfg, remote, owner, repo, dir, ref)
			}); err != nil {
				return nil, err
			}
		}

		if err := touchRepo(dir); err != nil {
			log.Printf("write refresh marker for %s: %v", dir, err)
		}
		return nil, nil
	})
	return err
}

func withUpdateSlot(ctx context.Context, fn func() error) error {
	if repoUpdateSem == nil {
		return fn()
	}
	select {
	case repoUpdateSem <- struct{}{}:
		defer func() { <-repoUpdateSem }()
		return fn()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// findReferenceRepo returns an already cached repository with the same
// repository name. Forks normally keep the original repository name, so this
// is enough to share their common objects without relying on GitHub's API.
func findReferenceRepo(store, remote, repo, exclude string) string {
	absStore, err := filepath.Abs(store)
	if err != nil {
		return ""
	}
	root := filepath.Join(absStore, remote)
	owners, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, owner := range owners {
		if !owner.IsDir() {
			continue
		}
		for _, name := range []string{repo, repo + ".git"} {
			p := filepath.Join(root, owner.Name(), name)
			if p == exclude || !repoExists(p) {
				continue
			}
			return p
		}
	}
	return ""
}

func cloneRepo(ctx context.Context, cfg Config, remote, owner, repo, dir, ref string) error {
	if ref != "" {
		err := cloneAttempt(ctx, cfg, remote, owner, repo, dir, ref)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A reference can be corrupt or on another filesystem. Fall back to a
		// normal clone instead of making the cache entry unavailable.
		log.Printf("clone %s with reference %s failed, retrying without reference: %v", repoURL(remote, owner, repo), ref, err)
	}
	return cloneAttempt(ctx, cfg, remote, owner, repo, dir, "")
}

func cloneAttempt(ctx context.Context, cfg Config, remote, owner, repo, dir, ref string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("create repository directory: %w", err)
	}

	base, err := os.MkdirTemp(cfg.StorePath, ".clone-*")
	if err != nil {
		return fmt.Errorf("create temporary clone directory: %w", err)
	}
	defer os.RemoveAll(base)
	tmp := filepath.Join(base, "repo")

	args := []string{"clone", "--bare"}
	if ref != "" {
		args = append(args, "--reference-if-able", ref)
	}
	args = append(args, repoURL(remote, owner, repo), tmp)
	if err := runGit(ctx, "", args...); err != nil {
		return err
	}
	if ref != "" {
		if err := adoptReferenceObjects(ref, tmp); err != nil {
			return fmt.Errorf("reuse objects from %s: %w", ref, err)
		}
	}
	if err := hardenRepo(ctx, tmp); err != nil {
		return err
	}
	if cfg.FetchPullRefs {
		if err := fetchPullRefs(ctx, tmp); err != nil {
			// Pull refs are an optional extra; a normal clone must still work.
			log.Printf("fetch pull refs for %s: %v", repoURL(remote, owner, repo), err)
		} else if err := touchPullRefs(tmp); err != nil {
			log.Printf("write pull-refs marker for %s: %v", tmp, err)
		}
	}
	if err := runGit(ctx, tmp, "update-server-info"); err != nil {
		return err
	}

	if repoExists(dir) {
		return nil
	}
	// Remove a leftover non-repository directory from an interrupted older
	// version. dir is derived from validated segments and is inside the store.
	if _, statErr := os.Stat(dir); statErr == nil {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove stale repository directory: %w", err)
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		return fmt.Errorf("install repository: %w", err)
	}
	return nil
}

// adoptReferenceObjects hard-links all object files from the reference into the
// new repository. Hard links keep the shared objects alive independently of the
// reference (unlike git alternates) and avoid copying pack files. If the object
// store is on a different filesystem, files are copied instead.
func adoptReferenceObjects(ref, dst string) error {
	alt := filepath.Join(dst, "objects", "info", "alternates")
	if _, err := os.Stat(alt); err != nil {
		if os.IsNotExist(err) {
			// git did not use the reference (for example because it is not a
			// valid repository); the clone is already complete.
			return nil
		}
		return err
	}
	if err := linkReferenceObjects(ref, dst); err != nil {
		return err
	}
	return os.Remove(alt)
}

func linkReferenceObjects(ref, dst string) error {
	srcRoot := filepath.Join(ref, "objects")
	if _, err := os.Stat(filepath.Join(srcRoot, "info", "alternates")); err == nil {
		// Linking only this repository's local objects would be incomplete;
		// fall back to a full clone rather than creating a broken cache entry.
		return fmt.Errorf("reference repository uses alternates")
	}

	return filepath.WalkDir(srcRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		top := strings.SplitN(rel, string(os.PathSeparator), 2)[0]
		if top == "pack" {
			// Pack files and their immutable indexes are safe to hard-link.
			// Mutable metadata such as multi-pack-index must stay per-repo.
			switch strings.TrimPrefix(filepath.Ext(rel), ".") {
			case "pack", "idx", "rev", "bitmap", "keep", "promisor":
			default:
				return nil
			}
		} else if !isHexObjectDir(top) {
			// Omit objects/info metadata (alternates, commit-graph, ...).
			return nil
		}

		dstPath := filepath.Join(dst, "objects", rel)
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return err
		}
		if fi, err := os.Stat(dstPath); err == nil {
			if fi.Mode().IsRegular() {
				return nil
			}
			return fmt.Errorf("destination %s is not a regular file", dstPath)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Link(path, dstPath); err != nil {
			if errors.Is(err, os.ErrExist) {
				return nil
			}
			if err := copyFile(path, dstPath); err != nil {
				return fmt.Errorf("link or copy %s: %w", rel, err)
			}
		}
		return nil
	})
}

func isHexObjectDir(s string) bool {
	if len(s) != 2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

// updateRepo fetches into the existing bare repository instead of deleting and
// re-cloning it. That keeps the cache available while it is being refreshed.
func updateRepo(ctx context.Context, cfg Config, dir string) error {
	// Old versions used --reference and could leave an alternates file that
	// points at another repository. Never serve objects through alternates.
	_ = os.Remove(filepath.Join(dir, "objects", "info", "alternates"))
	if err := hardenRepo(ctx, dir); err != nil {
		return err
	}

	if err := runGit(ctx, "",
		"-C", dir,
		"fetch", "--prune", "--tags", "origin",
		"+refs/heads/*:refs/heads/*",
		"+refs/tags/*:refs/tags/*",
	); err != nil {
		return err
	}
	if cfg.FetchPullRefs && pullRefsStale(dir, cfg.PullRefsTTL) {
		if err := fetchPullRefs(ctx, dir); err != nil {
			// A failed PR refresh must not make the normal repository
			// unavailable. The marker is left untouched so the next update
			// tries again.
			log.Printf("fetch pull refs for %s: %v", dir, err)
		} else if err := touchPullRefs(dir); err != nil {
			log.Printf("write pull-refs marker for %s: %v", dir, err)
		}
	}
	if err := runGit(ctx, dir, "update-server-info"); err != nil {
		return err
	}
	maybeGC(ctx, dir)
	return nil
}

func pullRefsStale(dir string, ttl time.Duration) bool {
	if ttl <= 0 {
		return true
	}
	fi, err := os.Stat(filepath.Join(dir, ".pull-refs-update"))
	return err != nil || time.Since(fi.ModTime()) >= ttl
}

func touchPullRefs(dir string) error {
	return os.WriteFile(filepath.Join(dir, ".pull-refs-update"), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
}

func fetchPullRefs(ctx context.Context, dir string) error {
	return runGit(ctx, "",
		"-C", dir,
		"fetch", "--prune", "--no-tags", "origin",
		"+refs/pull/*/head:refs/pull/*/head",
	)
}

// maybeGC keeps the number of pack files bounded. It is best effort: a failed
// GC must not make an otherwise usable cache unavailable.
func maybeGC(ctx context.Context, dir string) {
	entries, err := os.ReadDir(filepath.Join(dir, "objects", "pack"))
	if err != nil || len(entries) <= 500 {
		return
	}
	if err := runGit(ctx, dir, "gc"); err != nil {
		log.Printf("git gc %s: %v", dir, err)
	}
}

// hardenRepo disables dumb HTTP and pushes in the repository configuration.
// The CGI environment also sets these, but config works with older Git versions
// that do not understand GIT_CONFIG_COUNT.
func hardenRepo(ctx context.Context, dir string) error {
	if err := runGit(ctx, dir, "config", "http.receivepack", "false"); err != nil {
		return err
	}
	return runGit(ctx, dir, "config", "http.getanyfile", "false")
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, gitBinary, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s failed: %w: %s", strings.Join(args, " "), err, truncateOutput(string(out), 4096))
	}
	return nil
}

func truncateOutput(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "...(truncated)"
}

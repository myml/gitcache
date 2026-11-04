package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestValidateSegment(t *testing.T) {
	valid := []string{"owner", "repo.git", "a_b-c.d", "123"}
	for _, v := range valid {
		if err := validateSegment("segment", v); err != nil {
			t.Errorf("validateSegment(%q) = %v, want nil", v, err)
		}
	}

	invalid := []string{"", ".", "..", "a/b", `a\b`, "a b", "a%2Fb", "a\x00b", strings.Repeat("a", 256)}
	for _, v := range invalid {
		if err := validateSegment("segment", v); err == nil {
			t.Errorf("validateSegment(%q) = nil, want error", v)
		}
	}
}

func TestNormalizeOwnerRepo(t *testing.T) {
	owner, repo, err := normalizeOwnerRepo("Owner", "Repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "Owner" || repo != "Repo" {
		t.Fatalf("got %q/%q", owner, repo)
	}

	for _, tc := range [][2]string{
		{"..", "repo"},
		{"owner", ".."},
		{"owner", ".git"},
		{"owner/evil", "repo"},
		{"owner", "repo/evil"},
	} {
		if _, _, err := normalizeOwnerRepo(tc[0], tc[1]); err == nil {
			t.Errorf("normalizeOwnerRepo(%q, %q) = nil error", tc[0], tc[1])
		}
	}
}

func TestRepoStorePathContainment(t *testing.T) {
	store := t.TempDir()
	p, err := repoStorePath(store, "github.com", "owner", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, store+string(os.PathSeparator)) {
		t.Fatalf("path %q is outside store %q", p, store)
	}
	if _, err := repoStorePath(store, "..", "owner", "repo"); err == nil {
		t.Fatal("expected error for escaping remote")
	}
}

func TestValidGitAction(t *testing.T) {
	for _, a := range []string{"/info/refs", "/git-upload-pack", "/git-receive-pack"} {
		if !validGitAction(a) {
			t.Errorf("validGitAction(%q) = false", a)
		}
	}
	for _, a := range []string{"", "/", "/HEAD", "/objects/info/packs", "/../HEAD", "/info/refs/../../x", "info/refs"} {
		if validGitAction(a) {
			t.Errorf("validGitAction(%q) = true", a)
		}
	}
}

func TestAllowedReleaseURL(t *testing.T) {
	cfg := Config{AllowedReleaseHosts: parseHostSet("github.com,internal.example:8443")}

	tests := []struct {
		raw  string
		want bool
	}{
		{"https://github.com/a", true},
		{"https://github.com:443/a", true},
		{"http://github.com:80/a", true},
		{"https://internal.example:8443/a", true},
		{"https://github.com:8443/a", false},
		{"https://github.com.evil/a", false},
		{"http://127.0.0.1/a", false},
		{"ftp://github.com/a", false},
		{"https://user@github.com/a", false},
	}
	for _, tc := range tests {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.allowedReleaseURL(u); got != tc.want {
			t.Errorf("allowedReleaseURL(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}

	for _, raw := range []string{"github.com/a", "https://github.com/a#frag", "https://evil.example/a"} {
		if _, err := cfg.parseReleaseURL(raw); err == nil {
			t.Errorf("parseReleaseURL(%q) = nil error", raw)
		}
	}
}

func TestReleaseCacheKeyIncludesURL(t *testing.T) {
	meta := releaseMeta{ETag: `"etag"`, Length: 42}
	a := releaseCacheKey("https://github.com/a/asset", meta)
	b := releaseCacheKey("https://github.com/b/asset", meta)
	if a == b {
		t.Fatal("different URLs produced the same cache key")
	}
	if a != releaseCacheKey("https://github.com/a/asset", meta) {
		t.Fatal("cache key is not deterministic")
	}
	meta.Length = 43
	if a == releaseCacheKey("https://github.com/a/asset", meta) {
		t.Fatal("different lengths produced the same cache key")
	}
}

func TestReleaseCacheDoesNotMixAssets(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var mu sync.Mutex
	assets := map[string]struct {
		body string
		etag string
	}{
		"/a": {"AAAAA", `"a1"`},
		"/b": {"BBBBB", `"b1"`},
	}
	var getCount atomic.Int64

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asset, ok := assets[r.URL.Path]
		mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", asset.etag)
		w.Header().Set("Content-Length", strconv.Itoa(len(asset.body)))
		if r.Method == http.MethodGet {
			getCount.Add(1)
			_, _ = io.WriteString(w, asset.body)
		}
	}))
	defer upstream.Close()

	cfg := Config{
		StorePath:           t.TempDir(),
		AllowedReleaseHosts: parseHostSet(strings.TrimPrefix(upstream.URL, "http://")),
		HTTPTimeout:         5 * time.Second,
		DownloadTimeout:     5 * time.Second,
		MaxDownloadSize:     1 << 20,
		DownloadConcurrency: 2,
	}
	rc := newReleaseCache(cfg)
	router := gin.New()
	router.GET("/releases/*download_url", rc.handle)
	router.HEAD("/releases/*download_url", rc.handle)

	get := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/releases/"+target, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	wa := get(upstream.URL + "/a")
	if wa.Code != http.StatusOK || wa.Body.String() != "AAAAA" {
		t.Fatalf("/a: status=%d body=%q", wa.Code, wa.Body.String())
	}
	wb := get(upstream.URL + "/b")
	if wb.Code != http.StatusOK || wb.Body.String() != "BBBBB" {
		t.Fatalf("/b: status=%d body=%q", wb.Code, wb.Body.String())
	}
	if got := getCount.Load(); got != 2 {
		t.Fatalf("GET count = %d, want 2", got)
	}

	// A new ETag produces a new cache key and a fresh download.
	mu.Lock()
	assets["/a"] = struct {
		body string
		etag string
	}{"CCCCC", `"a2"`}
	mu.Unlock()
	wa2 := get(upstream.URL + "/a")
	if wa2.Code != http.StatusOK || wa2.Body.String() != "CCCCC" {
		t.Fatalf("/a after change: status=%d body=%q", wa2.Code, wa2.Body.String())
	}
}

func TestReleaseHeadDoesNotDownload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var getCount atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "5")
		w.Header().Set("ETag", `"x"`)
		if r.Method == http.MethodGet {
			getCount.Add(1)
		}
	}))
	defer upstream.Close()

	cfg := Config{
		StorePath:           t.TempDir(),
		AllowedReleaseHosts: parseHostSet(strings.TrimPrefix(upstream.URL, "http://")),
		HTTPTimeout:         5 * time.Second,
		DownloadTimeout:     5 * time.Second,
	}
	rc := newReleaseCache(cfg)
	router := gin.New()
	router.HEAD("/releases/*download_url", rc.handle)

	req := httptest.NewRequest(http.MethodHead, "/releases/"+upstream.URL+"/asset", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("HEAD body length = %d, want 0", w.Body.Len())
	}
	if got := getCount.Load(); got != 0 {
		t.Fatalf("HEAD triggered %d GET requests", got)
	}
}

func TestReleaseUpstreamErrorDoesNotLieAboutLength(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "10")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cfg := Config{
		StorePath:           t.TempDir(),
		AllowedReleaseHosts: parseHostSet(strings.TrimPrefix(upstream.URL, "http://")),
		HTTPTimeout:         5 * time.Second,
		DownloadTimeout:     5 * time.Second,
	}
	rc := newReleaseCache(cfg)
	router := gin.New()
	router.GET("/releases/*download_url", rc.handle)

	req := httptest.NewRequest(http.MethodGet, "/releases/"+upstream.URL+"/asset", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if got := w.Header().Get("Content-Length"); got == "10" {
		t.Fatal("error response kept the upstream Content-Length")
	}
}

func TestReleaseRejectsDisallowedHost(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := Config{
		StorePath:           t.TempDir(),
		AllowedReleaseHosts: parseHostSet("github.com"),
		HTTPTimeout:         5 * time.Second,
		DownloadTimeout:     5 * time.Second,
	}
	rc := newReleaseCache(cfg)
	router := gin.New()
	router.GET("/releases/*download_url", rc.handle)

	req := httptest.NewRequest(http.MethodGet, "/releases/http://127.0.0.1:1/secret", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestReleaseCacheEvictsOldFiles(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old")
	if err := os.WriteFile(old, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old+".meta", []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	rc := &releaseCache{cfg: Config{ReleaseCacheTTL: time.Hour}, dir: dir}
	rc.cleanup()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old cache file still exists: %v", err)
	}
	if _, err := os.Stat(old + ".meta"); !os.IsNotExist(err) {
		t.Fatalf("old metadata still exists: %v", err)
	}
}

func makeFakeBareRepo(t *testing.T, dir string) {
	t.Helper()
	for _, d := range []string{
		filepath.Join(dir, "objects", "pack"),
		filepath.Join(dir, "objects", "info"),
		filepath.Join(dir, "refs", "heads"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindReferenceRepo(t *testing.T) {
	store := t.TempDir()
	ownerRepo := filepath.Join(store, "github.com", "owner", "repo")
	forkRepo := filepath.Join(store, "github.com", "fork", "repo")
	oldGitRepo := filepath.Join(store, "github.com", "old", "repo.git")
	makeFakeBareRepo(t, ownerRepo)
	makeFakeBareRepo(t, forkRepo)
	makeFakeBareRepo(t, oldGitRepo)

	if got := findReferenceRepo(store, "github.com", "repo", filepath.Join(store, "github.com", "new", "repo")); got == "" {
		t.Fatal("expected a reference repository")
	}
	if got := findReferenceRepo(store, "github.com", "repo", ownerRepo); got != forkRepo {
		t.Fatalf("findReferenceRepo = %q, want %q", got, forkRepo)
	}
	if got := findReferenceRepo(store, "github.com", "missing", ""); got != "" {
		t.Fatalf("findReferenceRepo found %q for a missing repository", got)
	}

	oldStore := t.TempDir()
	oldGit := filepath.Join(oldStore, "github.com", "old", "repo.git")
	makeFakeBareRepo(t, oldGit)
	if got := findReferenceRepo(oldStore, "github.com", "repo", filepath.Join(oldStore, "github.com", "new", "repo")); got != oldGit {
		t.Fatalf("findReferenceRepo = %q, want old .git path %q", got, oldGit)
	}
}

func TestAdoptReferenceObjects(t *testing.T) {
	ref := t.TempDir()
	dst := t.TempDir()
	makeFakeBareRepo(t, ref)
	makeFakeBareRepo(t, dst)

	packPath := filepath.Join(ref, "objects", "pack", "pack-test.pack")
	if err := os.WriteFile(packPath, []byte("pack data"), 0o644); err != nil {
		t.Fatal(err)
	}
	looseDir := filepath.Join(ref, "objects", "ab")
	if err := os.MkdirAll(looseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	loosePath := filepath.Join(looseDir, "cdef")
	if err := os.WriteFile(loosePath, []byte("loose object"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ref, "objects", "info", "commit-graph"), []byte("graph"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ref, "objects", "pack", "multi-pack-index"), []byte("midx"), 0o644); err != nil {
		t.Fatal(err)
	}

	alt := filepath.Join(dst, "objects", "info", "alternates")
	if err := os.WriteFile(alt, []byte(filepath.Join(ref, "objects")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := adoptReferenceObjects(ref, dst); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{filepath.Join("pack", "pack-test.pack"), filepath.Join("ab", "cdef")} {
		srcInfo, err := os.Stat(filepath.Join(ref, "objects", rel))
		if err != nil {
			t.Fatal(err)
		}
		dstInfo, err := os.Stat(filepath.Join(dst, "objects", rel))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(srcInfo, dstInfo) {
			t.Errorf("%s was copied instead of hard-linked", rel)
		}
	}
	if _, err := os.Stat(alt); !os.IsNotExist(err) {
		t.Fatalf("alternates file still exists: %v", err)
	}
	for _, rel := range []string{
		filepath.Join("info", "commit-graph"),
		filepath.Join("pack", "multi-pack-index"),
	} {
		if _, err := os.Stat(filepath.Join(dst, "objects", rel)); !os.IsNotExist(err) {
			t.Fatalf("mutable metadata %s should not be copied: %v", rel, err)
		}
	}
}

func TestReferenceWithAlternatesIsRejected(t *testing.T) {
	ref := t.TempDir()
	dst := t.TempDir()
	makeFakeBareRepo(t, ref)
	makeFakeBareRepo(t, dst)
	if err := os.WriteFile(filepath.Join(ref, "objects", "info", "alternates"), []byte("/tmp/other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := linkReferenceObjects(ref, dst); err == nil {
		t.Fatal("expected an error for a reference repository that uses alternates")
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if gitBinary != "" {
		return
	}
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	gitBinary = p
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitBinary, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestUpdateRepoFetchesPullRefs(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	cache := filepath.Join(root, "cache.git")
	seed := filepath.Join(root, "seed")

	runTestGit(t, root, "init", "--bare", origin)
	runTestGit(t, root, "init", "--bare", cache)
	runTestGit(t, root, "init", "-b", "main", seed)
	runTestGit(t, seed, "config", "user.email", "test@example.com")
	runTestGit(t, seed, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(seed, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, seed, "add", "file.txt")
	runTestGit(t, seed, "commit", "-m", "initial")
	runTestGit(t, seed, "remote", "add", "origin", origin)
	runTestGit(t, seed, "push", "-q", "origin", "main")
	commit := runTestGit(t, seed, "rev-parse", "HEAD")
	runTestGit(t, root, "--git-dir", origin, "update-ref", "refs/pull/1/head", commit)
	runTestGit(t, cache, "remote", "add", "origin", origin)

	cfg := Config{FetchPullRefs: true, PullRefsTTL: 0}
	if err := updateRepo(context.Background(), cfg, cache); err != nil {
		t.Fatal(err)
	}
	if got := runTestGit(t, cache, "rev-parse", "refs/pull/1/head"); got != commit {
		t.Fatalf("refs/pull/1/head = %s, want %s", got, commit)
	}

	// A normal heads/tags refresh must not prune the cached PR ref.
	if err := updateRepo(context.Background(), Config{FetchPullRefs: false}, cache); err != nil {
		t.Fatal(err)
	}
	if got := runTestGit(t, cache, "rev-parse", "refs/pull/1/head"); got != commit {
		t.Fatalf("refs/pull/1/head after normal refresh = %s, want %s", got, commit)
	}
}

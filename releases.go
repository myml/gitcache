package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"
)

type releaseMeta struct {
	URL          string `json:"url"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	Length       int64  `json:"length"`
	ContentType  string `json:"content_type,omitempty"`
	Filename     string `json:"filename,omitempty"`
}

type upstreamError struct {
	Status int
	Err    error
}

func (e *upstreamError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("upstream status %d: %v", e.Status, e.Err)
	}
	return fmt.Sprintf("upstream status %d", e.Status)
}

func (e *upstreamError) Unwrap() error { return e.Err }

type releaseCache struct {
	cfg       Config
	client    *http.Client
	group     singleflight.Group
	sem       chan struct{}
	dir       string
	cleanupMu sync.Mutex
}

func newReleaseCache(cfg Config) *releaseCache {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	// Keep HEAD Content-Length and GET body length consistent. Release assets
	// are already compressed archives, so disabling transparent gzip is fine.
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = cfg.HTTPTimeout
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.IdleConnTimeout = 90 * time.Second
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 16

	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if !cfg.allowedReleaseURL(req.URL) {
				return fmt.Errorf("redirect to disallowed host %q", req.URL.Host)
			}
			return nil
		},
	}

	dir := filepath.Join(cfg.StorePath, "releases")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("create release cache directory: %v", err)
	}

	rc := &releaseCache{
		cfg:    cfg,
		client: client,
		sem:    make(chan struct{}, max(1, cfg.DownloadConcurrency)),
		dir:    dir,
	}
	go rc.cleanupLoop()
	return rc
}

func (rc *releaseCache) handle(c *gin.Context) {
	raw := strings.TrimPrefix(c.Param("download_url"), "/")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "download url is required"})
		return
	}

	target, err := rc.cfg.parseReleaseURL(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	headCtx, cancel := context.WithTimeout(c.Request.Context(), rc.cfg.HTTPTimeout)
	meta, err := rc.head(headCtx, target)
	cancel()
	if err != nil {
		rc.writeUpstreamError(c, err)
		return
	}

	key := releaseCacheKey(target.String(), meta)
	cachePath := filepath.Join(rc.dir, key)

	if st, statErr := os.Stat(cachePath); statErr == nil && !st.IsDir() {
		rc.serve(c, cachePath, meta)
		return
	}

	if c.Request.Method == http.MethodHead {
		// Do not download a whole asset just to answer HEAD.
		rc.writeHeadResponse(c, meta)
		return
	}

	var won bool
	var startedDownload bool
	_, err, _ = rc.group.Do(key, func() (interface{}, error) {
		won = true
		// Another request may have completed the download between our stat
		// check above and winning the singleflight call.
		if st, statErr := os.Stat(cachePath); statErr == nil && !st.IsDir() {
			return nil, nil
		}
		startedDownload = true
		return nil, rc.download(c, target, meta, cachePath)
	})
	if !won {
		// A concurrent request did the download. Wait for it, then serve the
		// completed file; never return a bogus 429 to other clients.
		if err != nil {
			rc.writeUpstreamError(c, err)
			return
		}
		if _, statErr := os.Stat(cachePath); statErr != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "download failed"})
			return
		}
		rc.serve(c, cachePath, meta)
		return
	}

	if err != nil {
		if !c.Writer.Written() {
			c.Writer.Header().Del("Content-Length")
			c.Writer.Header().Del("Content-Type")
			c.Writer.Header().Del("Content-Disposition")
			c.Writer.Header().Del("X-Content-Type-Options")
			rc.writeUpstreamError(c, err)
			return
		}
		// The response has already started; all we can do is stop and let the
		// client observe a truncated body. Do not cache a partial file.
		log.Printf("release download %s failed after response started: %v", target.Host, err)
		return
	}
	if !startedDownload {
		// The file appeared while we were waiting for singleflight; serve it
		// normally instead of sending an empty 200 response.
		rc.serve(c, cachePath, meta)
		return
	}
	if !c.Writer.Written() {
		c.Status(http.StatusOK)
	}
}

func (rc *releaseCache) head(ctx context.Context, target *url.URL) (releaseMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, target.String(), nil)
	if err != nil {
		return releaseMeta{}, err
	}
	req.Header.Set("User-Agent", "gitcache")
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := rc.client.Do(req)
	if err != nil {
		return releaseMeta{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return releaseMeta{}, &upstreamError{Status: resp.StatusCode}
	}

	return releaseMeta{
		URL:          target.String(),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		Length:       resp.ContentLength,
		ContentType:  resp.Header.Get("Content-Type"),
		Filename:     filenameFromURL(target),
	}, nil
}

// download writes the asset to a temporary file while best-effort streaming it
// to the first client. The temporary file is only renamed into the cache after
// the full body has been received and its size has been verified.
func (rc *releaseCache) download(c *gin.Context, target *url.URL, meta releaseMeta, cachePath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), rc.cfg.DownloadTimeout)
	defer cancel()

	// Waiting for a download slot is bounded by the client's request, but once
	// the download starts it continues even if the first client disconnects so
	// that waiting requests can still be served.
	select {
	case rc.sem <- struct{}{}:
		defer func() { <-rc.sem }()
	case <-c.Request.Context().Done():
		return c.Request.Context().Err()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "gitcache")
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := rc.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &upstreamError{Status: resp.StatusCode}
	}

	// Detect an asset changing between HEAD and GET. This also prevents
	// caching a body under a metadata key that describes different content.
	if meta.Length >= 0 && resp.ContentLength >= 0 && meta.Length != resp.ContentLength {
		return fmt.Errorf("asset changed between HEAD and GET (%d != %d)", meta.Length, resp.ContentLength)
	}
	if meta.ETag != "" && resp.Header.Get("ETag") != "" && meta.ETag != resp.Header.Get("ETag") {
		return fmt.Errorf("asset ETag changed between HEAD and GET")
	}

	expected := resp.ContentLength
	if expected < 0 {
		expected = meta.Length
	}
	if rc.cfg.MaxDownloadSize > 0 && expected > rc.cfg.MaxDownloadSize {
		return fmt.Errorf("asset exceeds maximum size")
	}

	tmp, err := os.CreateTemp(rc.dir, ".download-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	if c != nil {
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		if expected >= 0 {
			c.Header("Content-Length", strconv.FormatInt(expected, 10))
		}
		c.Header("Content-Type", contentType)
		c.Header("Content-Disposition", contentDisposition(meta.Filename))
		c.Header("X-Content-Type-Options", "nosniff")
	}

	var body io.Reader = resp.Body
	if rc.cfg.MaxDownloadSize > 0 {
		body = io.LimitReader(resp.Body, rc.cfg.MaxDownloadSize+1)
	}

	var dst io.Writer = tmp
	if c != nil {
		dst = io.MultiWriter(tmp, &bestEffortWriter{w: c.Writer})
	}
	n, err := io.Copy(dst, body)
	if err != nil {
		return fmt.Errorf("copy asset: %w", err)
	}
	if rc.cfg.MaxDownloadSize > 0 && n > rc.cfg.MaxDownloadSize {
		return fmt.Errorf("asset exceeds maximum size")
	}
	if expected >= 0 && n != expected {
		return fmt.Errorf("asset size mismatch: got %d, want %d", n, expected)
	}

	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, cachePath); err != nil {
		return fmt.Errorf("commit cached asset: %w", err)
	}

	meta.Length = n
	if meta.ContentType == "" {
		meta.ContentType = resp.Header.Get("Content-Type")
	}
	if meta.Filename == "" {
		meta.Filename = filenameFromURL(target)
	}
	if err := writeReleaseMeta(cachePath, meta); err != nil {
		log.Printf("write metadata for %s: %v", cachePath, err)
	}
	return nil
}

func (rc *releaseCache) serve(c *gin.Context, cachePath string, meta releaseMeta) {
	f, err := os.Open(cachePath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "open cached asset failed"})
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "stat cached asset failed"})
		return
	}

	if stored, ok := readReleaseMeta(cachePath); ok {
		if stored.ContentType != "" {
			meta.ContentType = stored.ContentType
		}
		if stored.Filename != "" {
			meta.Filename = stored.Filename
		}
		if stored.ETag != "" {
			meta.ETag = stored.ETag
		}
	}

	if meta.ContentType != "" {
		c.Header("Content-Type", meta.ContentType)
	}
	c.Header("Content-Disposition", contentDisposition(meta.Filename))
	c.Header("X-Content-Type-Options", "nosniff")
	if meta.ETag != "" {
		c.Header("ETag", meta.ETag)
		if ifNoneMatch(c.GetHeader("If-None-Match"), meta.ETag) {
			c.Status(http.StatusNotModified)
			return
		}
	}

	http.ServeContent(c.Writer, c.Request, meta.Filename, st.ModTime(), f)
}

func (rc *releaseCache) writeHeadResponse(c *gin.Context, meta releaseMeta) {
	if meta.Length >= 0 {
		c.Header("Content-Length", strconv.FormatInt(meta.Length, 10))
	}
	if meta.ContentType != "" {
		c.Header("Content-Type", meta.ContentType)
	}
	c.Header("Content-Disposition", contentDisposition(meta.Filename))
	c.Header("X-Content-Type-Options", "nosniff")
	if meta.ETag != "" {
		c.Header("ETag", meta.ETag)
	}
	c.Status(http.StatusOK)
}

func (rc *releaseCache) writeUpstreamError(c *gin.Context, err error) {
	status := http.StatusBadGateway
	var ue *upstreamError
	if errors.As(err, &ue) {
		switch ue.Status {
		case http.StatusForbidden, http.StatusNotFound, http.StatusGone, http.StatusTooManyRequests:
			status = ue.Status
		default:
			status = http.StatusBadGateway
		}
	}
	if status == http.StatusTooManyRequests {
		c.Header("Retry-After", "5")
	}
	log.Printf("release upstream error: %v", err)
	c.JSON(status, gin.H{"error": http.StatusText(status)})
}

func releaseCacheKey(rawURL string, meta releaseMeta) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s\n%d", rawURL, meta.ETag, meta.LastModified, meta.Length)
	return hex.EncodeToString(h.Sum(nil))
}

func writeReleaseMeta(cachePath string, meta releaseMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp := cachePath + ".meta.tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, cachePath+".meta"); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func readReleaseMeta(cachePath string) (releaseMeta, bool) {
	data, err := os.ReadFile(cachePath + ".meta")
	if err != nil {
		return releaseMeta{}, false
	}
	var meta releaseMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return releaseMeta{}, false
	}
	return meta, true
}

func filenameFromURL(u *url.URL) string {
	name := path.Base(u.Path)
	if name == "." || name == "/" || name == "" {
		return "download"
	}
	if decoded, err := url.PathUnescape(name); err == nil {
		name = decoded
	}
	name = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "download"
	}
	return name
}

func contentDisposition(filename string) string {
	if filename == "" {
		return "attachment"
	}
	v := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if v == "" {
		return "attachment"
	}
	return v
}

func ifNoneMatch(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || part == etag {
			return true
		}
	}
	return false
}

type bestEffortWriter struct {
	w io.Writer
}

func (b *bestEffortWriter) Write(p []byte) (int, error) {
	if b.w != nil {
		_, _ = b.w.Write(p)
	}
	return len(p), nil
}

func (rc *releaseCache) cleanupLoop() {
	rc.cleanup()
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rc.cleanup()
	}
}

func (rc *releaseCache) cleanup() {
	rc.cleanupMu.Lock()
	defer rc.cleanupMu.Unlock()

	entries, err := os.ReadDir(rc.dir)
	if err != nil {
		return
	}
	now := time.Now()
	type cacheItem struct {
		path string
		mod  time.Time
		size int64
	}
	var items []cacheItem
	var total int64

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		info, err := entry.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(rc.dir, name)

		switch {
		case strings.HasSuffix(name, ".tmp"):
			if now.Sub(info.ModTime()) > time.Hour {
				os.Remove(full)
			}
			continue
		case strings.HasSuffix(name, ".meta"), strings.HasSuffix(name, ".url"):
			// Metadata is removed together with its cache file. Old .url
			// sidecars from previous versions are simply obsolete.
			if strings.HasSuffix(name, ".url") && now.Sub(info.ModTime()) > time.Hour {
				os.Remove(full)
			}
			continue
		}

		if rc.cfg.ReleaseCacheTTL > 0 && now.Sub(info.ModTime()) > rc.cfg.ReleaseCacheTTL {
			os.Remove(full)
			os.Remove(full + ".meta")
			continue
		}
		items = append(items, cacheItem{path: full, mod: info.ModTime(), size: info.Size()})
		total += info.Size()
	}

	if rc.cfg.ReleaseCacheMaxBytes <= 0 || total <= rc.cfg.ReleaseCacheMaxBytes {
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.Before(items[j].mod) })
	for _, item := range items {
		if total <= rc.cfg.ReleaseCacheMaxBytes {
			break
		}
		if err := os.Remove(item.path); err == nil {
			os.Remove(item.path + ".meta")
			total -= item.size
		}
	}
}

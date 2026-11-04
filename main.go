package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
)

func main() {
	cfg := loadConfig()

	var err error
	if cfg.GitPath != "" {
		gitBinary = cfg.GitPath
	} else {
		gitBinary, err = exec.LookPath("git")
		if err != nil {
			log.Fatalf("git binary not found: %v", err)
		}
	}

	store, err := filepath.Abs(cfg.StorePath)
	if err != nil {
		log.Fatalf("resolve store path: %v", err)
	}
	cfg.StorePath = store
	if err := os.MkdirAll(filepath.Join(store, "releases"), 0o755); err != nil {
		log.Fatalf("create release cache: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(store, "repos"), 0o755); err != nil {
		log.Fatalf("create repo cache: %v", err)
	}

	repoUpdateSem = make(chan struct{}, max(1, cfg.CloneConcurrency))

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	if err := r.SetTrustedProxies(nil); err != nil {
		log.Printf("disable trusted proxies: %v", err)
	}

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	releases := newReleaseCache(cfg)
	r.GET("/releases/*download_url", releases.handle)
	r.HEAD("/releases/*download_url", releases.handle)

	git := gitHandler(cfg)
	r.GET("/:remote/:owner/:repo/*action", git)
	r.POST("/:remote/:owner/:repo/*action", git)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on %s (store %s)", cfg.ListenAddr, cfg.StorePath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		log.Fatalf("http server: %v", err)
	case sig := <-sigCh:
		log.Printf("received %s, shutting down", sig)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown: %v", err)
	}
}

func gitHandler(cfg Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		remote := strings.ToLower(strings.TrimSpace(c.Param("remote")))
		if !cfg.allowedRemote(remote) {
			c.JSON(http.StatusNotFound, gin.H{"error": "unknown remote"})
			return
		}

		owner, repo, err := normalizeOwnerRepo(c.Param("owner"), c.Param("repo"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid repository"})
			return
		}

		action := c.Param("action")
		if !validGitAction(action) {
			c.JSON(http.StatusNotFound, gin.H{"error": "unsupported git action"})
			return
		}

		repoPath, err := repoStorePath(cfg.StorePath, remote, owner, repo)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid repository path"})
			return
		}

		method := c.Request.Method
		shouldUpdate := method == http.MethodGet && action == "/info/refs"
		if shouldUpdate || !repoExists(repoPath) {
			// force is only true when a repository is missing; a POST never
			// needs to refresh an existing cache.
			if err := ensureRepo(c.Request.Context(), cfg, remote, owner, repo, repoPath, !shouldUpdate); err != nil {
				if repoExists(repoPath) {
					log.Printf("repository %s update failed, serving cached copy: %v", repoPath, err)
				} else {
					log.Printf("repository %s unavailable: %v", repoPath, err)
					c.JSON(http.StatusBadGateway, gin.H{"error": "clone repository failed"})
					return
				}
			}
		}

		if !repoExists(repoPath) {
			c.JSON(http.StatusNotFound, gin.H{"error": "repository not found"})
			return
		}

		lock := repoLock(repoPath)
		lock.RLock()
		defer lock.RUnlock()
		if !repoExists(repoPath) {
			c.JSON(http.StatusNotFound, gin.H{"error": "repository not found"})
			return
		}

		handler := &cgi.Handler{
			Path: gitBinary,
			Args: []string{"http-backend"},
			Env: []string{
				"GIT_PROJECT_ROOT=" + repoPath,
				"GIT_HTTP_EXPORT_ALL=1",
				"PATH_INFO=" + action,
				"REQUEST_METHOD=" + method,
				"QUERY_STRING=" + c.Request.URL.RawQuery,
				"CONTENT_TYPE=" + c.GetHeader("Content-Type"),
				// Defense in depth: even if a future route change lets dumb
				// HTTP paths through, do not serve arbitrary repo files and
				// do not accept pushes.
				"GIT_CONFIG_COUNT=2",
				"GIT_CONFIG_KEY_0=http.receivepack",
				"GIT_CONFIG_VALUE_0=false",
				"GIT_CONFIG_KEY_1=http.getanyfile",
				"GIT_CONFIG_VALUE_1=false",
			},
		}
		handler.ServeHTTP(c.Writer, c.Request)
	}
}

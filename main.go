package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"
)

var StorePath = "data"
var ListenAddr = ":8080"

// 执行命令, 并返回输出
func execCmd(cmd *exec.Cmd) error {
	log.Println("exec", cmd.Path, cmd.Args)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("exec '%s' faield. output: %s", strings.Join(cmd.Args, " "), out)
	}
	return nil
}

func main() {
	if path := os.Getenv("STORE_PATH"); len(path) > 0 {
		StorePath = path
	}
	if addr := os.Getenv("LISTEN_ADDR"); len(addr) > 0 {
		ListenAddr = addr
	}
	m := gin.Default()
	m.GET("releases/*download_url", cacheRelease())
	var group singleflight.Group
	m.Any(":remote/:owner/:repo/*action", func(ctx *gin.Context) {
		repoPath := filepath.Join(StorePath, ctx.Param("remote"), ctx.Param("owner"), ctx.Param("repo"))
		repoPath, err := filepath.Abs(repoPath)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if ctx.Request.Method == http.MethodGet && ctx.Param("action") == "/info/refs" {
			_, err, _ := group.Do(repoPath, func() (interface{}, error) {
				for i := 0; i < 3; i++ {
					err := clone(ctx.Param("remote"), ctx.Param("owner"), ctx.Param("repo"))
					if err == nil {
						return nil, nil
					}
					log.Println("clone repo error: ", err)
					time.Sleep(time.Second)
				}
				return nil, fmt.Errorf("clone repo failed")
			})
			if err != nil {
				log.Println("Error cloning repository: ", err)
				ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}

		handler := &cgi.Handler{
			Path: "git",
			Args: []string{"http-backend"},
			Env: []string{
				"GIT_PROJECT_ROOT=" + repoPath,
				"GIT_HTTP_EXPORT_ALL=",
				"REQUEST_METHOD=" + ctx.Request.Method,
				"PATH_INFO=" + ctx.Param("action"),
				"QUERY_STRING=" + ctx.Request.URL.RawQuery,
				"CONTENT_TYPE=" + ctx.GetHeader("Content-Type"),
			},
		}
		handler.ServeHTTP(ctx.Writer, ctx.Request)
	})
	err := m.Run(ListenAddr)
	if err != nil {
		log.Fatal(err)
	}
}

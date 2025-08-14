package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
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
	var lock sync.Mutex
	m := gin.Default()
	m.GET("releases/*download_url", cacheRelease())
	m.GET(":remote/:owner/:repo/*file", func(ctx *gin.Context) {
		if ctx.Param("file") == "/info/refs" {
			lock.Lock()
			defer lock.Unlock()
			err := clone(ctx.Param("remote"), ctx.Param("owner"), ctx.Param("repo"))
			if err != nil {
				log.Println("Error cloning repository: ", err)
				ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}
		http.FileServer(http.Dir(StorePath)).ServeHTTP(ctx.Writer, ctx.Request)
	})
	err := m.Run(ListenAddr)
	if err != nil {
		log.Fatal(err)
	}
}

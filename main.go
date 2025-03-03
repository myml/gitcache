package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

var StorePath = "data"
var ListenAddr = ":8080"

func execCmd(cmd *exec.Cmd) error {
	log.Println("exec", cmd.Path, cmd.Args)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("exec '%s' faield. output: %s", strings.Join(cmd.Args, " "), out)
	}
	return nil
}

func findSameName(name string) (string, error) {
	remoteList, err := os.ReadDir(StorePath)
	if err != nil {
		return "", fmt.Errorf("find same name: %w", err)
	}
	for i := range remoteList {
		ownerList, err := os.ReadDir(path.Join(StorePath, remoteList[i].Name()))
		if err != nil {
			return "", fmt.Errorf("find same name: %w", err)
		}
		for j := range ownerList {
			repoPath := path.Join(StorePath, remoteList[i].Name(), ownerList[j].Name(), name)
			_, err := os.Stat(repoPath)
			if err == nil {
				return repoPath, nil
			}
		}
	}
	return "", os.ErrNotExist
}

func main() {
	if path := os.Getenv("STORE_PATH"); len(path) > 0 {
		StorePath = path
	}
	if addr := os.Getenv("LISTEN_ADDR"); len(addr) > 0 {
		ListenAddr = addr
	}

	m := gin.Default()
	m.GET(":remote/:owner/:repo/info/refs", func(ctx *gin.Context) {
		remote := ctx.Param("remote")
		owner := ctx.Param("owner")
		repo := ctx.Param("repo")
		url := fmt.Sprintf("https://%s/%s/%s", remote, owner, repo)
		localRepo := fmt.Sprintf("store/%s/%s/%s", remote, owner, repo)
		tempRepo := fmt.Sprintf("store/%s/%s/%s.tmp", remote, owner, repo)
		referenceRepo := localRepo

		existsLocalRepo := false
		_, err := os.Stat(localRepo)
		if err == nil {
			existsLocalRepo = true
		} else {
			sameNameRepo, err := findSameName(repo)
			if err == nil {
				referenceRepo = sameNameRepo
			}
		}
		err = os.RemoveAll(tempRepo)
		if err != nil {
			_ = ctx.AbortWithError(http.StatusServiceUnavailable, err)
			return
		}
		args := []string{"clone", "--bare", "--dissociate", "--reference-if-able", referenceRepo, url, tempRepo}
		err = execCmd(exec.Command("git", args...))
		if err != nil {
			_ = ctx.AbortWithError(http.StatusServiceUnavailable, err)
			return
		}
		cmd := exec.Command("git", "update-server-info")
		cmd.Dir = tempRepo
		err = execCmd(cmd)
		if err != nil {
			_ = ctx.AbortWithError(http.StatusServiceUnavailable, err)
			return
		}
		if existsLocalRepo {
			err = os.RemoveAll(localRepo)
			if err != nil {
				_ = ctx.AbortWithError(http.StatusServiceUnavailable, err)
				return
			}
		}
		err = os.Rename(tempRepo, localRepo)
		if err != nil {
			_ = ctx.AbortWithError(http.StatusServiceUnavailable, err)
			return
		}
		http.FileServer(http.Dir(StorePath)).ServeHTTP(ctx.Writer, ctx.Request)
	})
	m.NoRoute(func(ctx *gin.Context) {
		http.FileServer(http.Dir(StorePath)).ServeHTTP(ctx.Writer, ctx.Request)
	})
	err := m.Run(":8080")
	if err != nil {
		log.Fatal(err)
	}
}

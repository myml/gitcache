package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"

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
			if strings.HasSuffix(name, ".git") {
				name := strings.Replace(name, ".git", "", -1)
				repoPath := path.Join(StorePath, remoteList[i].Name(), ownerList[j].Name(), name)
				_, err := os.Stat(repoPath)
				if err == nil {
					return repoPath, nil
				}
			} else {
				repoPath := path.Join(StorePath, remoteList[i].Name(), ownerList[j].Name(), name+".git")
				_, err := os.Stat(repoPath)
				if err == nil {
					return repoPath, nil
				}
			}
		}
	}
	return "", os.ErrNotExist
}

func clone(remote, owner, repo string) error {
	url := fmt.Sprintf("https://%s/%s/%s", remote, owner, repo)
	localRepo := fmt.Sprintf("%s/%s/%s/%s", StorePath, remote, owner, repo)
	tempRepo := fmt.Sprintf("%s/%s/%s/%s.tmp", StorePath, remote, owner, repo)
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
		return fmt.Errorf("Failed to remove temporary repository directory: %w", err)
	}
	args := []string{"clone", "--bare", "--dissociate", "--reference-if-able", referenceRepo, url, tempRepo}
	err = execCmd(exec.Command("git", args...))
	if err != nil {
		return fmt.Errorf("Failed to clone repository: %w", err)
	}
	cmd := exec.Command("git", "update-server-info")
	cmd.Dir = tempRepo
	err = execCmd(cmd)
	if err != nil {
		return fmt.Errorf("Failed to update server info: %w", err)
	}
	if existsLocalRepo {
		err = os.RemoveAll(localRepo)
		if err != nil {
			return fmt.Errorf("Failed to remove local repository directory: %w", err)
		}
	}
	err = os.Rename(tempRepo, localRepo)
	if err != nil {
		return fmt.Errorf("Failed to rename temporary repository directory: %w", err)
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
	m.GET(":remote/:owner/:repo/*file", func(ctx *gin.Context) {
		if ctx.Param("file") == "/info/refs" {
			lock.Lock()
			defer lock.Unlock()
			err := clone(ctx.Param("remote"), ctx.Param("owner"), ctx.Param("repo"))
			if err != nil {
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

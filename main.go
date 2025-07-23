package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
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

func copySymlink(src, dest string) error {
	files, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read dir(%s): %w", src, err)
	}
	for i := range files {
		fname := files[i].Name()
		err = os.Link(filepath.Join(src, fname), filepath.Join(dest, fname))
		if err != nil {
			return fmt.Errorf("create symlink(%s): %w", dest, err)
		}
	}
	return nil
}

func clone(remote, owner, repo string) error {
	url := fmt.Sprintf("https://%s/%s/%s", remote, owner, repo)
	localRepo := fmt.Sprintf("%s/%s/%s/%s", StorePath, remote, owner, repo)
	tempRepo := fmt.Sprintf("%s/%s/%s/%s.tmp", StorePath, remote, owner, repo)
	referenceRepo := localRepo
	existsLocalRepo := false
	existsRefRepo := false
	_, err := os.Stat(localRepo)
	if err == nil {
		existsRefRepo = true
		existsLocalRepo = true
	} else {
		sameNameRepo, err := findSameName(repo)
		if err == nil {
			existsRefRepo = true
			referenceRepo = sameNameRepo
		}
	}
	err = os.RemoveAll(tempRepo)
	if err != nil {
		return fmt.Errorf("Failed to remove temporary repository directory: %w", err)
	}
	args := []string{"clone", "--bare", "--reference-if-able", referenceRepo, url, tempRepo}
	err = execCmd(exec.Command("git", args...))
	if err != nil {
		return fmt.Errorf("Failed to clone repository: %w", err)
	}
	if existsRefRepo {
		err = copySymlink(
			filepath.Join(referenceRepo, "objects/pack"),
			filepath.Join(tempRepo, "objects/pack"),
		)
		if err != nil {
			return err
		}
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
	if existsRefRepo {
		err = os.Remove(filepath.Join(tempRepo, "objects/info/alternates"))
		if err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("remove alternates: %w", err)
			}
		}
	}
	err = os.Rename(tempRepo, localRepo)
	if err != nil {
		return fmt.Errorf("Failed to rename temporary repository directory: %w", err)
	}
	return nil
}

func cacheRelease() gin.HandlerFunc {
	var memCache sync.Map
	return func(ctx *gin.Context) {
		url := ctx.Param("download_url")[1:]
		if cacheFilePath, ok := memCache.Load(url); ok {
			log.Printf("Serving cached file %s", cacheFilePath)
			ctx.File(cacheFilePath.(string))
		}
		resp, err := http.Get(url)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		defer resp.Body.Close()
		changeid := resp.Header.Get("ETag")
		if len(changeid) == 0 {
			changeid = resp.Header.Get("Last-Modified")
		}
		cachekey := fmt.Sprintf("%s-%s", resp.Header.Get(url), changeid)
		data := sha256.Sum256([]byte(cachekey))
		cacheStoreKey := hex.EncodeToString(data[:])
		cacheFilePath := path.Join(StorePath, "release", cacheStoreKey)
		ctx.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", path.Base(cacheFilePath)))
		memCache.Store(url, cacheFilePath)
		if _, err := os.Stat(cacheFilePath); err == nil {
			log.Printf("Serving cached file %s", cacheFilePath)
			ctx.File(cacheFilePath)
			return
		}
		if err := os.MkdirAll(path.Dir(cacheFilePath), 0755); err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create cache directory"})
			return
		}
		out, err := os.Create(cacheFilePath)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create cache file"})
			return
		}
		defer out.Close()
		if _, err := out.ReadFrom(io.TeeReader(resp.Body, ctx.Writer)); err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to write to cache file"})
			return
		}
		err = os.WriteFile(cacheFilePath+".url", []byte(url+"\n"+cacheStoreKey), 0644)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to write URL to cache file"})
			return
		}
		log.Printf("Cached %s to %s", url, cacheFilePath)
	}
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

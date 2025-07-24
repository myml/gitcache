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

func genCacheStoreKey(url string) (string, int64) {
	resp, err := http.Head(url)
	if err != nil {
		log.Panic(err)
	}
	defer resp.Body.Close()
	changeid := resp.Header.Get("ETag")
	if len(changeid) == 0 {
		changeid = resp.Header.Get("Last-Modified")
	}
	cachekey := fmt.Sprintf("%s-%s-%d", resp.Header.Get(url), changeid, resp.ContentLength)
	data := sha256.Sum256([]byte(cachekey))
	cacheStoreKey := hex.EncodeToString(data[:])
	return cacheStoreKey, resp.ContentLength
}

func cacheRelease() gin.HandlerFunc {
	var memCache sync.Map
	return func(ctx *gin.Context) {
		url := ctx.Param("download_url")[1:]
		// 检查内存缓存
		if cacheFilePath, ok := memCache.Load(url); ok {
			log.Printf("Serving cached file %s", cacheFilePath)
			ctx.File(cacheFilePath.(string))
		}
		// 检查磁盘缓存
		cacheStoreKey, contentLength := genCacheStoreKey(url)
		ctx.Header("Content-Length", fmt.Sprintf("%d", contentLength))
		cacheFilePath := path.Join(StorePath, "releases", cacheStoreKey)
		if _, err := os.Stat(cacheFilePath); err == nil {
			memCache.Store(url, cacheFilePath)
			log.Printf("Serving cached file %s", cacheFilePath)
			ctx.File(cacheFilePath)
			return
		}
		var out *os.File
		var resp *http.Response
		if stat, err := os.Stat(cacheFilePath + ".tmp"); err == nil {
			// 如果缓存文件大小与文件大小相同，则移动缓存文件
			if stat.Size() == contentLength {
				err = os.Rename(cacheFilePath+".tmp", cacheFilePath)
				if err != nil {
					ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to rename cache file"})
					return
				}
				memCache.Store(url, cacheFilePath)
				log.Printf("Serving cached file %s", cacheFilePath)
				ctx.File(cacheFilePath)
				return
			}
			// 断点续传
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create request"})
				return
			}
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", stat.Size()))
			resp, err = http.DefaultClient.Do(req)
			if err != nil {
				ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to download file"})
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusPartialContent {
				ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to download file"})
				return
			}
			{
				// 将缓存文件中的内容发送到客户端
				f, err := os.OpenFile(cacheFilePath+".tmp", os.O_RDONLY, 0644)
				if err != nil {
					ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open cache file"})
					return
				}
				defer f.Close()
				if _, err := io.Copy(ctx.Writer, f); err != nil {
					ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to copy cache file"})
					return
				}
				f.Close()
			}
			out, err = os.OpenFile(cacheFilePath+".tmp", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create cache file"})
				return
			}
			defer out.Close()
		} else {
			// 新下载
			resp, err = http.Get(url)
			if err != nil {
				ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to download file"})
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to download file"})
				return
			}
			if err := os.MkdirAll(path.Dir(cacheFilePath), 0755); err != nil {
				ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create cache directory"})
				return
			}
			out, err = os.Create(cacheFilePath + ".tmp")
			if err != nil {
				ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create cache file"})
				return
			}
			defer out.Close()
		}
		log.Println("Downloading", url)
		if _, err := out.ReadFrom(io.TeeReader(resp.Body, ctx.Writer)); err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to write to cache file"})
			return
		}
		err := os.WriteFile(cacheFilePath+".url", []byte(url+"\n"+cacheStoreKey), 0644)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to write URL to cache file"})
			return
		}
		err = os.Rename(cacheFilePath+".tmp", cacheFilePath)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to rename cache file"})
			return
		}
		memCache.Store(url, cacheFilePath)
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

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"sync"

	"github.com/gin-gonic/gin"
)

func genCacheStoreKey(client *http.Client, url string) (key string, contentLength int64, supportRange bool) {
	resp, err := client.Head(url)
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
	return cacheStoreKey, resp.ContentLength, resp.Header.Get("Accept-Ranges") == "bytes"
}

func cacheRelease() gin.HandlerFunc {
	client := &http.Client{}
	if proxyUrl := os.Getenv("HTTP_PROXY"); len(proxyUrl) > 0 {
		proxyUrl, err := url.Parse(proxyUrl)
		if err != nil {
			log.Fatal(err)
		}
		log.Println("proxyUrl", proxyUrl)
		client.Transport = &http.Transport{
			Proxy: http.ProxyURL(proxyUrl),
		}
	}
	var memCache sync.Map
	var downloading sync.Map

	return func(ctx *gin.Context) {
		if len(ctx.Param("download_url")) == 0 {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "download_url is required"})
			return
		}
		url := ctx.Param("download_url")[1:]
		// 检查内存缓存
		if v, ok := memCache.Load(url); ok {
			log.Printf("Serving cached file %s", v)
			cacheFilePath := v.(string)
			if _, err := os.Stat(cacheFilePath); err == nil {
				ctx.File(cacheFilePath)
				return
			}
		}
		// 检查磁盘缓存
		cacheStoreKey, contentLength, supportRange := genCacheStoreKey(client, url)
		ctx.Header("Content-Length", fmt.Sprintf("%d", contentLength))
		cacheFilePath := path.Join(StorePath, "releases", cacheStoreKey)
		if _, err := os.Stat(cacheFilePath); err == nil {
			memCache.Store(url, cacheFilePath)
			log.Printf("Serving cached file %s", cacheFilePath)
			ctx.File(cacheFilePath)
			return
		}
		// 检查是否正在下载，如果正在下载，则返回429
		if _, ok := downloading.Load(url); ok {
			ctx.JSON(http.StatusTooManyRequests, gin.H{"error": "url is downloading"})
			return
		}
		downloading.Store(url, true)
		defer downloading.Delete(url)
		// 下载文件
		var out *os.File
		var resp *http.Response
		if stat, err := os.Stat(cacheFilePath + ".tmp"); err == nil && supportRange {
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
			resp, err = client.Do(req)
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
			resp, err = client.Get(url)
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

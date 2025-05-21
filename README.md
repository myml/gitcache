# Git 服务器

## 简介

这是一个基于 Go 语言的 Git 代理缓存服务器。使用缓存引用实现增量拉取数据，减少网络数据传输。

## 安装

通过 golang

```bash
go install github.com/myml/gitcache
export STORE_PATH=/path/to/repositories   # 默认值: ./data
export LISTEN_ADDR=:8080
gitcache
```

通过 docker

docker run -p 8080:8080 -v gitcache:/data ghcr.io/myml/gitcache:latest

## 使用

git clone http://127.0.0.1:8080/github.com/myml/github.com/github/github-mcp-server

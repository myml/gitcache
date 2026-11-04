# Git 代理缓存

一个基于 Go 的 Git 仓库和 GitHub Release 资源缓存代理。

## 特性

- 缓存裸仓库，首次从上游克隆，之后使用 `git fetch` 增量更新
- 同一上游主机下的同名仓库（典型场景是 fork）首次克隆时可复用已缓存的对象，减少重复下载和磁盘占用
- 可选缓存 GitHub PR 引用（`refs/pull/*/head`），支持通过代理执行 `git fetch origin pull/123/head`
- 缓存 GitHub Release 下载资源，支持 `Range` 断点续传
- 上游主机白名单，避免把服务变成任意 URL/任意仓库的开放代理
- 同一资源的并发请求合并为一次下载，不再互相返回 `429`
- 仓库更新期间仍可继续提供已有的缓存副本

## 安装

通过 Go：

```bash
go install github.com/myml/gitcache

export STORE_PATH=/path/to/data        # 默认 ./data
export LISTEN_ADDR=:8080               # 默认 :8080
export ALLOWED_REMOTES=github.com      # 默认只允许 github.com
gitcache
```

通过 Docker：

```bash
docker run -p 8080:8080 \
  -e STORE_PATH=/data \
  -v gitcache:/data \
  ghcr.io/myml/gitcache:latest
```

Docker 镜像默认以 root 运行，`/data` 可直接写入，兼容旧数据卷。如果希望以非 root 运行，可以自行准备数据目录权限，并在 `docker run` 时加 `--user 10001:10001`。

## 使用

克隆仓库：

```bash
git clone http://127.0.0.1:8080/github.com/myml/gitcache
```

路径格式为 `/<上游主机>/<owner>/<repo>`。如果上游不是 `github.com`，必须把它加入 `ALLOWED_REMOTES`，例如：

```bash
ALLOWED_REMOTES=github.com,git.example.com
```

拉取 PR：

```bash
git fetch origin pull/123/head:pr-123
git checkout pr-123
```

这要求代理已经缓存了对应的 `refs/pull/123/head`；普通 `git clone` 不会自动 checkout PR，需要先 fetch。

下载 Release 资源：

```bash
curl -O http://127.0.0.1:8080/releases/https://github.com/owner/repo/releases/download/v1.0.0/asset.tar.gz
```

`/releases/` 后面的完整 URL 也可以做 URL 编码。默认只允许从 `github.com`、`objects.githubusercontent.com`、`release-assets.githubusercontent.com`、`github-releases.githubusercontent.com`、`codeload.github.com` 获取资源，重定向目标同样会校验。需要代理其他域名时设置：

```bash
ALLOWED_RELEASE_HOSTS=github.com,objects.githubusercontent.com,example.com
```

## 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `STORE_PATH` | `data` | 缓存根目录 |
| `LISTEN_ADDR` | `:8080` | HTTP 监听地址 |
| `ALLOWED_REMOTES` | `github.com` | 允许的 Git 上游主机（逗号分隔，可带端口） |
| `ALLOWED_RELEASE_HOSTS` | `github.com,...` | 允许的 Release 上游主机（逗号分隔，可带端口） |
| `REPO_TTL` | `0` | 仓库缓存的刷新间隔；`0` 表示每次 `info/refs` 都刷新（默认保持最新） |
| `OBJECT_REUSE` | `true` | 首次克隆时是否复用同一主机下同名仓库的对象；只缓存公开仓库时开启，缓存私有仓库时建议设为 `false` |
| `FETCH_PULL_REFS` | `false` | 是否缓存 GitHub PR 引用 `refs/pull/*/head`；开启后可通过代理拉取 PR，但会额外缓存所有 PR 对象 |
| `PULL_REFS_TTL` | `0` | PR 引用的刷新间隔；`0` 表示每次刷新仓库时都更新，与 `REPO_TTL` 默认行为一致 |
| `CLONE_TIMEOUT` | `10m` | 单次 clone/fetch 超时 |
| `HTTP_TIMEOUT` | `30s` | Release HEAD 请求和响应头超时 |
| `DOWNLOAD_TIMEOUT` | `30m` | 单个 Release 资源下载超时 |
| `MAX_DOWNLOAD_SIZE` | `2147483648` | 单个资源最大字节数；`0` 表示不限制 |
| `DOWNLOAD_CONCURRENCY` | `4` | 同时下载的 Release 资源数 |
| `CLONE_CONCURRENCY` | `4` | 同时 clone/fetch 的仓库数 |
| `RELEASE_CACHE_TTL` | `0` | Release 缓存保留时间；`0` 表示不按时间清理 |
| `RELEASE_CACHE_MAX_BYTES` | `0` | Release 缓存总大小上限；`0` 表示不限制 |
| `GIT_PATH` | 自动查找 | 指定 `git` 可执行文件路径 |

## 行为说明

- 仓库第一次访问时执行 `git clone --bare`，之后在同一个裸仓库上执行 `git fetch --prune`，不会在读取过程中删除并替换整个目录。
- 首次 clone 时，如果缓存中已经有同一上游主机、同一仓库名的仓库（例如 fork），会先用它作为 `--reference`，再把对象文件硬链接到新仓库，最后删除新仓库的 `alternates`；因此 fork 之间可以共享公共历史对象。
- 同一个仓库的后续更新仍然在原裸仓库上 `git fetch`，不会重新 clone。
- 不同仓库之间硬链接的是整个对象文件，不是按对象内容去重；如果同名仓库其实无关，也会共享文件（公开仓库场景可以接受）。设置 `OBJECT_REUSE=false` 可关闭该行为。
- 更新失败时，只要本地已有缓存，就继续提供旧副本；只有本地完全没有缓存时才返回错误。
- 仓库缓存默认 `REPO_TTL=0`，每次 `info/refs` 都会尝试 `git fetch`，因此 clone 能拿到最新提交；如果更在意减少上游请求，可以设成 `5m` 之类的值。
- 默认不缓存 PR 引用。设置 `FETCH_PULL_REFS=true` 后，首次 clone 和后续刷新会把 `refs/pull/*/head` 一并缓存；`PULL_REFS_TTL=0` 表示每次刷新仓库时都更新 PR 引用，与 `REPO_TTL=0` 的默认行为一致。PR 引用可能很多，大仓库可以调大该值或关闭。
- Release 缓存文件按 URL、ETag、Last-Modified 和 Content-Length 计算 key，避免不同 URL 长度相同造成的缓存串内容。
- 只支持 Git Smart HTTP 的 `info/refs`、`git-upload-pack` 和 `git-receive-pack`，不支持 dumb HTTP 文件访问；`git-receive-pack` 默认被禁用。
- 只支持裸仓库缓存，不支持 push。

## 从旧版本升级

旧版本按仓库名复用 `--reference` 并硬链接对象；新版本默认恢复了同样的 fork 对象复用，因此旧缓存可以继续使用，不需要为了兼容性清空。旧缓存中的 `objects/info/alternates` 会在下一次 `fetch` 时被清理。

如果缓存中包含私有仓库，建议设置 `OBJECT_REUSE=false` 并清空一次旧缓存，避免不同仓库之间共享对象。

## 安全提示

- 服务本身没有认证。请只监听在受信任的网络中，或放在带认证的反向代理之后。
- 不要把 `ALLOWED_REMOTES` 或 `ALLOWED_RELEASE_HOSTS` 配成任意主机；白名单是防止 SSRF/开放代理的主要边界。
- `OBJECT_REUSE=true` 会让不同仓库共享对象文件，只适合缓存公开仓库；如果同一个 `STORE_PATH` 中会缓存私有仓库，请设为 `false`。
- 当前不包含仓库级磁盘配额，请结合 `RELEASE_CACHE_MAX_BYTES`、定期清理或文件系统配额使用。

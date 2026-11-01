# 用 Docker / Compose 部署

> 返回 [README](../README.md) · 飞牛用户请走 [fnOS 应用包](install-fnos.md)

适用：任何装了 Docker 的 Linux 机器（NAS / 树莓派 / 云主机）。

## 方式一：预构建镜像（最省事）

提供 **linux/amd64 + linux/arm64** 双架构镜像，同时发布在 Docker Hub 与 GHCR：

```bash
docker run -d --name autodocker \
  --restart unless-stopped \
  -p 17777:17777 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/data:/data" \
  -e TZ=Asia/Shanghai \
  -e SELF_CONTAINER=autodocker \
  haihaipypy/autodocker:latest
```

启动后访问 `http://<你的机器IP>:17777`。

> 默认用 Docker Hub。拉不动时换 GHCR：`ghcr.io/haihaipypy/autodocker:latest`，内容一致。

**默认不设密码、不填通知凭据**，起来就能用。要设密码加一行 `-e ADMIN_PASSWORD='你的密码'`；
要通知进「设置」页选渠道、填凭据（[飞书](feishu.md) / [Telegram](telegram.md)）。

> 端口 **17777**。换端口要两处一起改：`-p 宿主机:容器内` 的两侧，以及 `-e PORT=`。

| 标签 | 含义 |
| --- | --- |
| `latest` | **最近一次打 tag 的正式版本**（跟着 `v*` tag 走，不跟 main） |
| `1.2.3` / `1.2` | 对应 `v1.2.3` 这个 tag，想锁死版本、不跟着自动往前时用 |
| `main` | main 分支最新构建，紧跟开发进度，可能夹带未完成的改动 |
| `sha-1a2b3c4` | 对应提交，回滚排查用 |

> Docker Hub 匿名拉取按 IP 限流（每 6 小时 100 次），升级频繁时可以 `docker login` 后拉。
> 拉不动就走方式三本地构建。

## 方式二：Compose（推荐，不用源码）

把仓库根目录的 `docker-compose.yml` 拷到任意目录就能跑：

```bash
mkdir -p ~/autodocker && cd ~/autodocker
curl -fsSL -o docker-compose.yml \
  https://raw.githubusercontent.com/haihaipypy/autodocker/main/docker-compose.yml
docker compose up -d
```

或者自己建一份最小版：

```yaml
services:
  autodocker:
    container_name: autodocker
    image: haihaipypy/autodocker:latest
    restart: unless-stopped
    ports:
      - "17777:17777"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./data:/data
    environment:
      - TZ=Asia/Shanghai
      - PORT=17777
      - SELF_CONTAINER=autodocker
```

`ADMIN_PASSWORD` 与全部通知凭据（`FEISHU_*` / `TELEGRAM_*`）默认留空：不设密码即不启用登录，
不填凭据就不推通知。在网页「设置」里补齐即可，环境变量只作首次启动的初值，数据库里的值优先。

首次启动会扫描一次，并自动把本机所有带 tag 的镜像纳入监控。
访问 `http://<IP>:17777`。

换端口改 compose 里两处：`ports:` 的 `"17777:17777"` 与 `PORT=17777`。

**升级**：`docker compose pull && docker compose up -d`，配置与数据都不动。
`latest` 只跟着正式版本（`v*` tag）走，main 上有半成品也不会被它带上。
想锁死版本就把 `:latest` 换成具体版本号（如 `:1.2.3`），代价是每次升级要手动改这一行。

## 方式三：从源码本地构建

只有改了代码要测才需要这条路 —— 它要整个仓库，**光有 compose 文件是构建不出来的**。

```bash
git clone https://github.com/haihaipypy/autodocker.git
cd autodocker
# 打开 docker-compose.yml，注释掉 image: 那一行、解开 build: .
docker compose up -d --build
```

> 如果看到 `failed to read dockerfile: open Dockerfile: no such file or directory`，
> 就是这一类问题：当前目录里没有 `Dockerfile`。要么把 `image:` 用回去（不需要构建），
> 要么 `git clone` 整个仓库再构建。单独拷一份 compose 文件是构建不了的。

## 关键配置项

| 变量 | 说明 |
| --- | --- |
| `ADMIN_PASSWORD` | 网页访问密码。**默认空 = 不启用登录**；挂了 docker.sock，要暴露到公网就一定设上。之后可在网页「设置」里改，数据库里的值优先 |
| `ADMIN_USER` | 登录用户名，默认 `admin` |
| `SELF_CONTAINER` | 自己的容器名，用于阻止「更新自己」。默认 `autodocker`，改了容器名要同步改这里 |
| `SCAN_INTERVAL` | 巡检间隔秒数，最小 30，`0` 表示只在手动点击时扫描 |
| `NOTIFY_CHANNEL` | 设置页默认展示哪个渠道的表单：`feishu` / `telegram` / `dingtalk`。**不决定消息发给谁** —— 凭据齐全的渠道都会收到，每个渠道在网页里另有开关 |
| `FEISHU_APP_ID` / `FEISHU_APP_SECRET` | 飞书自建应用凭据。作为**首次启动的初值**，之后在网页「设置」里维护，数据库里的值优先 |
| `FEISHU_CHAT_ID` | 接收通知的群 ID，同上 |
| `TELEGRAM_BOT_TOKEN` | Telegram bot 凭据（`@BotFather` 给的 `数字:字母` 串），同上 |
| `TELEGRAM_CHAT_ID` | 接收消息的会话 ID。群与频道为负数；也可在设置页点「读取会话」自动填 |
| `TELEGRAM_API_BASE` | Telegram API 地址，留空用 `https://api.telegram.org`。直连不通时填自建反代 |
| `TELEGRAM_PROXY` | HTTP 代理，形如 `http://192.168.1.2:7890`。走代理上外网时填，留空直连 |
| `DINGTALK_CLIENT_ID` / `DINGTALK_CLIENT_SECRET` | 钉钉企业内部应用的 Client ID / Secret，同上。Secret **只显示一次** |
| `DINGTALK_CARD_TEMPLATE_ID` | 卡片平台上已发布模板的 ID（不是模板名称） |
| `DINGTALK_CONVERSATION_ID` | 接收卡片的群会话 ID，也可在设置页点「读取会话」自动填 |
| `AUTO_UPDATE` | 发现即自动更新（无人值守）。默认关闭，即「发现 → 通知 → 等人确认」 |
| `WATCH` | 额外监控本机没有运行的镜像，逗号分隔，例如 `nginx:1.25,redis:7.2` |
| `ROLLBACK_ON_FAILURE` | 更新失败是否自动回滚，默认 `true` |
| `KEEP_OLD_CONTAINER` | 更新成功后是否保留旧容器（默认删除）。调试期建议开 |
| `HEALTH_CHECK_WAIT` | 新容器启动后的健康检查观察窗口（秒），默认 20 |
| `REGISTRY_MIRROR` | Docker Hub 加速地址，可填多个、逗号分隔、按序回退，例如 `docker.1ms.run,docker.xuanyuan.me`。留空时按「守护进程 `registry-mirrors` → 内置默认」依次兜底。**只作用于 Docker Hub**，ghcr.io / quay.io / 私有仓库仍直连 |
| `REGISTRY_AUTH` | 私有仓库凭据，形如 `docker.io=user:pass,ghcr.io=ghp_xxx` |
| `REGISTRY_INSECURE` | 允许 http 明文注册表。`127.0.0.1` / `localhost` / `::1` 这类回环地址**不需要打开它**，会自动走 http（与 Docker 守护进程的默认规则一致）；打开后则是**所有**源都改用 http，Docker Hub 与公共加速站会因此整片失败，只在必要时刻临时开 |
| `STATIC_DIR` | 指向 `web/` 目录以覆盖内嵌前端，改前端时免重新编译 |

## 关于容器本身

- 挂载两处：`/var/run/docker.sock`（必需，监控与重建靠它）与 `/data`（SQLite 数据库，
  不挂载的话重建容器后配置会丢）。
- 容器以 root 运行，自带 `HEALTHCHECK`。
- 镜像里只有二进制、`ca-certificates` 与 `tzdata`，前端已编进二进制。

## 本机开发

```bash
# 后端（默认 17777；飞牛应用里由 run.sh 指定为 19999）
go run ./cmd/server

# 前端是纯静态文件，改完刷新浏览器即可；
# 也可以指定 STATIC_DIR 指向 web/ 目录来覆盖内嵌资源：
STATIC_DIR=./web go run ./cmd/server
```

本地构建镜像：

```bash
docker build -t autodocker:dev .
docker run --rm -p 17777:17777 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/data:/data" autodocker:dev
```

`.dockerignore` 排掉了 `fnos/`（里面有 13 MB 的 linux 二进制）、`*.fpk`、
`tools/`、`.workbuddy/`，构建上下文只有源码，几秒就能传完。

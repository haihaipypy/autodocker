# AutoDocker

**Docker 镜像更新监测 · 飞书 / Telegram / 钉钉确认 · 一键自动重建**

[![Release](https://img.shields.io/github/v/release/haihaipypy/autodocker?label=release&color=blue)](https://github.com/haihaipypy/autodocker/releases/latest)
[![Docker Pulls](https://img.shields.io/docker/pulls/haihaipypy/autodocker)](https://hub.docker.com/r/haihaipypy/autodocker)
[![License](https://img.shields.io/github/license/haihaipypy/autodocker)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/haihaipypy/autodocker)](go.mod)

监测本机容器的镜像有没有新版本，推送到飞书、Telegram 或钉钉。在聊天里点「立即更新」，服务按原配置重建容器，
挂载、端口、网络、环境变量全部保留，失败自动回滚。没人用的镜像也能一键清理。

![AutoDocker](docs/images/cover.png)

---

## 它解决什么问题

| 场景 | 传统做法 | AutoDocker |
| --- | --- | --- |
| 想知道镜像有没有更新 | 手动 `docker pull` 比对，或装个全自动更新的又不放心 | 定时巡检，发现更新推送到飞书 / Telegram / 钉钉 |
| 决定要不要更新 | 登录 NAS / SSH 上去点 | 在聊天消息里点「立即更新」 |
| 更新容器 | 手写 `docker run`，容易漏掉挂载或环境变量 | 按原配置重建，字段零丢失 |
| 更新把服务搞挂了 | 手动找回原配置重新起 | 健康检查不通过自动回滚 |
| 磁盘被旧镜像占满 | 一个个 `docker rmi`，还怕删错正在用的 | 标明使用状态，一键清理未使用的 |

## 特性

- **通知渠道可选**：飞书（企业自建应用，长连接收回调）、Telegram（bot 长轮询）、
  钉钉（企业内部应用，Stream 长连接 + 互动卡片）。填好凭据的渠道都会收到提醒，可以同时用三个。
  每个渠道有独立开关，关掉只停推送、配置保留。设置页左栏选渠道，右栏是所选渠道的完整配置流程。
- **容器与镜像分开管理**：容器页看「在跑的东西要不要更新」，镜像页看「磁盘上有什么、能不能清」。
- **两种检测模式自动判定**：浮动 tag 比摘要；版本号 tag 额外巡检 tag 列表，出现更高版本时提示。
- **按钮按状态渲染**，不摆灰按钮；检测失败的原因直接写在容器名下方。
- **两个忽略粒度**：卡片上「忽略本版本」只放行这一次远端内容，作者推了新版本照常提醒；
  容器页「忽略」是整条监控项不再检测。
- **深浅主题可切换**，默认深色。
- **聊天里点按钮后页面自动刷新结果**。
- **会检查自己有没有新版本**：每天看一眼，有就在顶栏提示「新版本」，点开能看到更新内容与升级步骤
  （飞牛包 / Docker 镜像两套，命令可一键复制）。只提示不自动升级，关掉开关就完全不联网。
  直连 GitHub 不通时自动换公共加速，不需要配置。

## 安装

三种方式任选。

### 1. 飞牛 fnOS 应用包

从 [Releases](https://github.com/haihaipypy/autodocker/releases/latest) 下载 `Autodocker.fpk`，
应用中心 → 手动安装 → 选中它。

装完桌面出现「Docker 更新助手」，也可直接访问 `http://<NAS_IP>:19999`。

→ [飞牛安装与排障](docs/install-fnos.md)：自己打包、ARM 机型、运行身份

### 2. Docker Compose（推荐）

在任意目录建一个 `docker-compose.yml`：

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

```bash
docker compose up -d
```

打开 `http://<IP>:17777`。需要飞书通知就进「设置」里填凭据。

仓库根目录的 `docker-compose.yml` 是同一份，带全部环境变量注释。

→ [Docker / Compose 部署与全部环境变量](docs/install-docker.md)

### 3. docker run

```bash
docker run -d --name autodocker --restart unless-stopped \
  -p 17777:17777 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/data:/data" \
  -e TZ=Asia/Shanghai -e SELF_CONTAINER=autodocker \
  haihaipypy/autodocker:latest
```

要密码就加 `-e ADMIN_PASSWORD='你的密码'`。换端口要把宿主机端口和容器内 `PORT` 一起改，
比如 `-p 18000:18000 -e PORT=18000`。

Docker Hub 拉不动时换 GHCR：`ghcr.io/haihaipypy/autodocker:latest`，内容一致。

镜像为 `linux/amd64` / `linux/arm64` 双架构。升级：`docker compose pull && docker compose up -d`。

## 使用

![界面](docs/images/ui-dark.png)

定时巡检 → 发现新版本推送到飞书 / Telegram / 钉钉 → 点「立即更新」→ 拉镜像、按原配置重建、健康检查
→ 通过则删旧容器，不通过自动回滚。

「可用」列写的是仓库里更高的版本号；没有更高版本号但同一 tag 被重新推送过，写「内容已更新」。

→ [完整使用说明](docs/usage.md)：两个列表的边界、忽略粒度、检测模式、清理策略

## 通知配置

三个渠道任选其一或全用，都在「设置 → 通知渠道」里配。填好凭据的那个渠道测通后开关会自动打开；
关掉开关只停「推新提醒」，配置一项不动，已发出的卡片也照常回写状态。

### 飞书

**必须是企业自建应用。群自定义机器人（webhook）收不到按钮点击。**

六步：建应用 → 开机器人能力 → 申请 `im:message:send_as_bot`（应用身份）→
在「**回调配置**」标签页用长连接订阅 `card.action.trigger` → **发布新版本** →
把机器人拉进群，最后在设置页填 App ID / Secret / chat_id。

两个容易漏的点：回调加在旁边的「事件配置」里不生效；权限或回调改完不发布新版本，
点按钮报 `200340`，但卡片照收。

→ [飞书配置逐图步骤](docs/feishu.md)

### Telegram

五步：`@BotFather` 发 `/newbot` 拿 Token → 填进设置页**先保存一次** →
把 bot 拉进群并 **`@它` 说句话**（或直接私聊它发一条）→ 点「读取会话」，点一下会话即填入并保存 →
发送测试消息并**真的点一下**按钮。

三个容易漏的点：Token 不保存服务就不会开始长轮询，读不到会话 ID；
bot 默认读不到群里的普通聊天（成员列表写「无权读取消息」），得 `@它` 或关掉隐私模式；
这个 bot 若在别处设过 webhook 就收不到长轮询结果（两者互斥）。

> 国内直连 `api.telegram.org` 多半不通，「读不到会话」十有八九是这原因。
> 设置页有 **「检测连通性」**，不通时填**代理地址**（`192.168.1.2:7890` 或 `socks5://…`）或**API 地址**（自建反代）。

收到提醒时长这样，点按钮直接重建，不用回网页：

![Telegram 里收到的新版本提醒](docs/images/telegram-card-update.png)

→ [Telegram 配置全流程](docs/telegram.md)

### 钉钉

**同样必须是企业内部应用。群机器人（webhook）只能单向推消息，收不到按钮点击。**

还要认清入口：后台「应用开发」下有个和「**钉钉应用**」并列的「机器人」，那是旧模型，
钉钉官方已说明「将逐渐退出历史舞台，不要用它来创建」。卡片模板关联、权限、版本发布
都在「钉钉应用」详情页里，从应用入口建才不用来回绕。

主线是六步：建企业内部应用 → 开机器人能力 → 在卡片平台搭一张模板并**发布**
（变量名用 `title` / `status` / `body`，按钮回传参数用 `{"action":"update"}` 这类 JSON）
→ 拿 `卡片模板 ID` 与 `Client ID / Secret` → 用 Stream 长连接订阅卡片回调 → 把机器人拉进群。

机器人那张配置表**除「消息接收模式」外全是必填**：名称、图标（建议 240×240）、
简介（≤10 字符）、描述（≤200 字符）、消息预览图（png/jpg，≤2M），最后模式选 **Stream**。
项目自带 `fnos/ICON_256.PNG` 可直接当图标。

三个容易漏的点：卡片**模板必须发布**，草稿态的模板没法投放；
按钮的回传参数是「一层 JSON 字符串」，写成裸 `update` 是取不到的；
机器人配置改完要去「版本管理与发布」发新版本，否则群里看到的还是旧信息。

钉钉卡片更新只改数据、不换整条消息，所以点完按钮后按钮会留在卡片上（飞书和 Telegram 是换掉整条）。

会话 ID **必须让机器人在群里收到过一次消息**才有（钉钉没有反查群列表的接口）。
记下来之后会存进数据库，重启、装新包都不用再 @，只有加新群才需要再来一次。

→ [钉钉配置逐图步骤](docs/dingtalk.md)

## 安全

- 挂载 `/var/run/docker.sock` 等同于宿主机 root 权限，只在可信内网使用。
- 访问密码默认空即不启用登录。要让端口离开内网，务必在「设置」里配上密码。
- 服务会拒绝更新自身，也会拒绝更新带 `--rm` 的容器。

→ [安全须知与已知边界](docs/faq.md)

## 技术简述

- 纯 Go 单二进制，前端零构建（`go:embed`），SQLite 用 `modernc.org/sqlite`（免 CGO）。
- 不依赖 Docker 官方 SDK，直接封装 Engine REST API，`HostConfig` 原样透传。
- 三个渠道全是长连接：飞书走 WebSocket、Telegram 走 bot 长轮询、钉钉走 Stream ——
  都免公网、免备案、免反向代理。
- 注册表加速三层兜底：用户设置 → 守护进程 `registry-mirrors` → 内置默认，只作用于 Docker Hub。

→ [技术实现与目录结构](docs/architecture.md)

## 许可与致谢

MIT，见 [LICENSE](LICENSE)。

本项目为独立实现，`go.mod` 只依赖飞书 SDK 与纯 Go SQLite 驱动，业务代码全部自研。
[vigil](https://github.com/jingyuan9527/vigil) 与
[dockerCopilot](https://github.com/onlyLTY/dockerCopilot)（均 AGPL-3.0）仅作思路参考，未复制其代码。

## 文档索引

| 文档 | 内容 |
| --- | --- |
| [install-fnos.md](docs/install-fnos.md) | 飞牛应用打包、安装、运行身份、排障 |
| [install-docker.md](docs/install-docker.md) | Docker / Compose 部署、全部环境变量、本地开发 |
| [feishu.md](docs/feishu.md) | 飞书自建应用配置全流程（含截图） |
| [telegram.md](docs/telegram.md) | Telegram bot 配置全流程、收不到消息的排查 |
| [dingtalk.md](docs/dingtalk.md) | 钉钉企业内部应用、卡片模板、Stream 长连接的配置全流程 |
| [usage.md](docs/usage.md) | 两个列表的边界、忽略粒度、检测模式、界面 |
| [architecture.md](docs/architecture.md) | 技术选型、重建保真规则、存储坑位、测试 |
| [faq.md](docs/faq.md) | 安全须知、已知边界、常见故障排查 |
| [release.md](docs/release.md) | 版本号、CI、Docker Hub / GHCR、Release 上传 |

---

作者：**无辣** · [blog.1day.vip](https://blog.1day.vip)

# 多节点监控方案（远程服务器 / NAS）

> 状态：v1.7.0 已实现「只读取」部分 —— 节点接入、上报、展示、在线状态、手动巡检。
> v1.7.5 起 Agent 只扫容器维度（原因见 §一 末尾）。
> 远程一键更新、远端通知、批量操作放在 v1.8.0。
> 目标：一台主控统一监控多台服务器 / NAS 上的容器，更新提醒汇到同一处，卡片按钮仍能一键更新。
> 约束：被监控端做到「跑一条命令，此后再也不用管」。

## 实现进度（v1.7.0）

| 能力 | 状态 | 说明 |
|---|---|---|
| 节点增删改查、令牌签发与吊销 | ✅ | 令牌明文只展示一次，库里存 sha256 |
| `curl \| sh` 一键安装 | ✅ | 脚本由 Hub 吐出，地址内嵌，幂等（重跑即升级） |
| WebSocket 长连接、注册、心跳、重连 | ✅ | 30s 心跳，90s 无帧断线，指数退避 1s→60s |
| 全量上报容器快照 | ✅ | 每轮巡检后整份替换，不做增量 |
| 节点页展示（状态 / 系统 / 版本 / 容器数 / 待更新数） | ✅ | 点开可看该节点的容器列表；连上但还没交过快照时显示「巡检中…」，不显示 0 |
| 手动触发远端巡检 | ✅ | `POST /api/nodes/{id}/scan` |
| Agent 日志回看 | ✅ | 最近 120 行随上报送出（页面上还没做展示，日志在 `Report.Logs` 里） |
| 远端更新状态推通知 | ⏳ v1.8.0 | 这一版远端只上报、不发通知，避免「点不了按钮」的割裂 |
| 一键远程重建容器 | ⏳ v1.8.0 | 指令通道与白名单已就位，只接了 `scan` |
| 批量更新、一键升级节点 | ⏳ v1.9.0 | — |

## 一、结论先行

**同一份二进制，双角色运行。主控（Hub）接受连接，被监控端（Agent）主动连出去。**

- Agent 侧跑完整巡检：读本机 Docker、查仓库版本、判定状态、保留原有重建逻辑。
- Hub 侧只做三件事：聚合各节点状态、统一发通知、把卡片按钮的点击路由回对应节点。
- 通信走 **Agent 出站的 WebSocket 长连接**，目标机不开任何入站端口、不存 SSH 密钥。

**Agent 是彻底无感的**：不监听端口、没有网页、没有配置文件、不需要 SSH 登录。安装就是在目标机粘一条 Hub 生成的命令，之后开机自启、断线自愈、升级从 Hub 点一下。

**Agent 只扫容器维度**（`scanner.Options.SkipImages`）：「本机没被使用的镜像」这一维度在节点页没有入口，而它的检测是串行的、每个 tag 都要访问一次 registry —— 在访问 Docker Hub 需要代理的机器上，几十个闲置镜像足以把首轮巡检拖上几十分钟，这期间 Hub 一条数据都收不到（上报在整轮扫完之后）。跳过它之后首轮巡检在多数机器上几十秒内完成。容器维度仍然并发 4 路。

## 二、三条路线对比

| | A. 主控 SSH 到目标机 | B. Agent 反向长连接（推荐） | C. 主控统一检测 |
|---|---|---|---|
| 目标机要装东西 | 不用（但要开 SSH + 有 docker CLI） | **一条命令装个容器** | 一条命令装个容器 |
| 目标机要开入站端口 | 是（22） | **否** | 否 |
| 主控持有敏感凭据 | SSH 私钥 / 密码 | 可吊销的节点令牌 | 可吊销的节点令牌 |
| NAT / 内网机 | 要端口映射 | **天然可用** | 天然可用 |
| 重建容器保真 | 要用 shell 重写一遍 `buildSpec`（约 600 行 Go） | **直接复用现有代码** | 直接复用（但要下发指令） |
| 私有仓库凭据 | 要另存 | 用目标机已有的 docker login | 拿不到 |
| 仓库查询压力 | 集中在主控 | **分散在各节点** | 集中在主控 |
| 改判定规则 | — | 要升级各节点 | 改一处生效 |
| 目标机离线 | 主控不知道 | 心跳自动发现 | 心跳自动发现 |

**为什么排除 SSH**：这个工具的核心价值是「按原配置重建容器」，那套逻辑（镜像引用覆盖、hostname 清理、匿名卷补回、内置网络剔除、健康检查回滚）是几百行 Go，用 shell 重写必然保真度下降。SSH 唯一省下的是「装程序」这一步，代价却是把等同于 root 的私钥存在 SQLite 里。不划算。

**为什么排除「暴露 Docker TCP」**：`dockerx` 确实支持 `tcp://`，主控配一下 `DOCKER_HOST` 就能直接管，代码零改动。但 2375 明文端口等于把 root 摆在网络上，而内网机还得做端口映射。这条只适合「同内网、临时接一台」，不做正式功能。

**和 CF-Server-Monitor 的关系**：借鉴它的「Hub 生成安装命令 + Agent 跑在后台无前台」这个形态；不借鉴它的「纯单向、无指令通道」——我们必须要能远程触发更新，所以多一条受控的指令下行。

## 三、架构

```
                 ┌─────────────────────────────┐
                 │  Hub（飞牛 NAS / 常开的一台）  │
                 │  本机 node_id = 0            │
                 │  · 节点管理 / 在线状态         │
                 │  · 聚合展示                   │
                 │  · 唯一通知出口               │
                 │  · 卡片按钮 → 指令路由         │
                 │  · 生成安装命令               │
                 └──────────┬──────────────────┘
                            │  WebSocket（Agent 主动连出，TLS）
          ┌─────────────────┼─────────────────┐
          │                 │                 │
   ┌──────┴─────┐    ┌──────┴─────┐    ┌──────┴─────┐
   │ Agent 群晖  │    │ Agent VPS  │    │ Agent 别的  │
   │ 无端口      │    │ 无端口      │    │ 无端口      │
   │ docker.sock│    │ docker.sock│    │ docker.sock│
   └────────────┘    └────────────┘    └────────────┘
```

### 职责划分（关键决策）

| 环节 | 谁做 | 为什么 |
|---|---|---|
| 列举容器 / 读本地摘要 | Agent | 只有它能读到自己那台机器的 `docker.sock` |
| 查仓库有没有新版 | Agent | 用目标机已有的 docker 登录态，私有仓库才通；同时把查询压力分散，不会因为节点变多把「拉取超时」的问题放大 |
| 判定状态 / 去重 | Agent | 现有 `scanner` + `notified_tag` / `notified_digest` 整套逻辑原样复用，一行不改 |
| 重建容器 | Agent | 复用 `updater` 的保真重建与回滚 |
| 发通知 | **只有 Hub** | 一台机器只配一套通知渠道；卡片投递坐标集中在一处，改状态时不用跨平台对账 |
| 卡片按钮路由 | Hub | 按通知上的 `node_id` 决定是自己执行还是下发 |

**Agent 侧不发通知**是现成能力：`scanner.notify()` 里 `notifier == nil` 时只落库不推送。Agent 模式下不注入 notifier 即可，不需要加开关。

## 四、目标机侧：零感知

### Agent 的形态

| 项 | 做法 |
|---|---|
| 监听端口 | **一个都不开**，纯出站连接 |
| 前端页面 | 无。`web/` 的内嵌资源在 Agent 模式下不加载 |
| 配置文件 | 无。全部通过环境变量传入 |
| 日志查看 | `docker logs -f autodocker-agent`，或直接在 Hub 页面上看（见下） |
| 开机自启 | Docker 的 `--restart=unless-stopped` |
| 崩溃拉起 | 同上 |
| 断线重连 | Agent 内置指数退避，1s → 60s |
| 升级 | 重跑同一条安装命令（Agent 版本跟随 Hub） |
| 卸载 | `docker rm -f autodocker-agent && docker volume rm autodocker-agent-data` |

**不开端口这件事值得单独说**：NAS 上装东西最烦的就是端口冲突和防火墙规则。Agent 一个端口都不占，装完就等于不存在。

### 安装：一条命令

Hub 页面点「添加节点」→ 填名字 + **必填 Hub 访问地址** → 生成令牌 → 页面给出这条命令（地址和令牌都已内嵌）：

```bash
curl -fsSL https://hub.1day.vip/api/agent/install.sh | sh -s -- --token=<32位十六进制令牌>
```

脚本**不拉镜像**，而是从 Hub 取程序本体（`GET /api/agent/binary`，用节点令牌鉴权），挂进一个 alpine 容器里跑。这么做有两个直接好处：

- **版本永远一致**：Agent 跑的就是 Hub 这一份字节，Hub 是 1.7.4，装上去就是 1.7.4。不会出现「`latest` 指向旧版本、装完不工作」。
- **目标机不必访问 Docker Hub**：国内 NAS 拉镜像常要代理或加速器，这条路直接绕开；只需要能拉到 `alpine:3.24`（约 3 MB）。

架构限制：镜像里没有别的架构的产物，Hub 只能下发**与自己同架构**的那一份。x86 的 Hub 管 ARM 的 NAS 时，命令末尾加 `--image` 退回镜像方式：

```bash
curl -fsSL https://hub.1day.vip/api/agent/install.sh | sh -s -- --token=<令牌> --image
```

### Hub 访问地址：必填，由用户判断

这是生成安装命令的**前置条件**：没填或填错格式，「生成」按钮就是灰的。

- 字段预填当前访问 Hub 用的地址（`r.Host`），用户可以直接用，也可以改成别的。
- 必须是 `http://` 或 `https://` 开头的完整地址，后端按前缀校验，不合法直接拒绝生成。
- 装在内网的机器填局域网地址，装在公网的机器填公网域名。**由用户判断，程序不做猜测**。
- **地址能不能通，填的时候就要自己确认**——目标机连不上这个地址，装上去也是一直重连。Hub 不做「帮你试试通不通」这种半吊子校验，公网地址在 Hub 本机测出来的结果没有意义。

每个节点单独存这份地址（`nodes.hub_url`）。所以同一套 Hub 可以给内网机器发内网命令、给外网机器发公网命令，互不干扰；节点列表里也显示这个地址，出问题时一眼看出当初是按哪条路装的。

为什么不做自动探测：Agent 装在哪、能不能走内网，只有用户知道。与其让程序试错再回退，不如把这步摆到明面上——**填一次，后面全自动**。

### 脚本做什么（幂等，重复执行 = 升级）

1. 检查 `docker` 命令存在、daemon 在跑，不然直接报错退出，不留半截状态。
2. `uname -m` 判断架构，从 Hub 下载对应程序到 `/var/lib/autodocker-agent/autodocker`。先写 `.new` 再改名——下载中断留下的半个可执行文件会让下次启动直接崩，且报错和下载毫无关系。
3. 基础镜像 `alpine:3.24` 不存在就拉（只 3 MB）。
4. 若已存在 `autodocker-agent` 容器 → `docker rm -f`（**data 卷保留**，令牌和本地库都在里面）。
5. `docker run -d --restart=unless-stopped`，挂 `docker.sock`、data 卷和那个程序文件，环境变量注入 Hub 地址、令牌、宿主机 `hostname` 作为节点名。
6. 打印「节点已启动，回到 Hub 页面点刷新即可看到」。

不装依赖、不写 systemd unit、不动宿主机的任何配置文件。重复执行就是升级：程序文件被覆盖，容器重建，数据卷不动。

### 最终落到目标机的那个容器

主路径（程序由 Hub 下发，挂进 alpine）：

```bash
docker run -d --name autodocker-agent --restart=unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v autodocker-agent-data:/data \
  -v /var/lib/autodocker-agent/autodocker:/app/autodocker:ro \
  --entrypoint /app/autodocker \
  -e AUTODOCKER_MODE=agent \
  -e AUTODOCKER_HUB=https://hub.1day.vip \
  -e AUTODOCKER_TOKEN=<令牌> \
  -e AUTODOCKER_NODE_NAME="$(hostname)" \
  alpine:3.24
```

跨架构时退回镜像（`--image`）：

```bash
docker run -d --name autodocker-agent --restart=unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v autodocker-agent-data:/data \
  -e AUTODOCKER_MODE=agent \
  -e AUTODOCKER_HUB=https://hub.1day.vip \
  -e AUTODOCKER_TOKEN=<令牌> \
  -e AUTODOCKER_NODE_NAME="$(hostname)" \
  haihaipypy/autodocker:1.7.4
```

系统信息（发行版、内核）从 `docker info` 里读，不用额外挂 `/etc/os-release`。

### 日志不用 SSH 上去看

Agent 在每次上报里带一段最近 200 行的环形缓冲，Hub 的节点详情页直接展开。出问题时不用登机器。

### 升级策略

**默认：Hub 页面一键升级。** 检测到节点版本落后，节点列表右边出现「可升级」，点一下对所有在线节点下发 `self_upgrade`，各节点自己完成替换。

自动化程度刻意留了一个缺口——**完全自动升级默认关闭**。原因是这个动作要 `docker rm -f` 掉自己：一旦新镜像拉不动或起不来，节点直接永久失联，只能本人上机救。让用户点一下，比事后救机器便宜得多。开关放在节点设置里，能接受风险的人自己打开。

如果连点一下都懒得点，重跑那条 `curl | sh` 也是升级——脚本第 3 步就是「删旧容器、保留数据卷」。

### 安全边界必须写在文档里

这条命令等价于把该机器的 root 交给 Hub：挂着 `docker.sock` 的容器，能做宿主机上任何事。所以：

- 只装在信任的机器上；
- 令牌单节点绑定、可单独吊销；
- Agent 只认 `scan` / `update` / `ignore` / `snooze` / `mode` / `self_upgrade` 六条指令，**没有任何「执行任意命令」的入口**；
- Hub 侧建议开 `ADMIN_PASSWORD` 并且只在 TLS 后面暴露。

## 五、数据模型

新增两张表，**不改现有 `images` 表、不动 `UNIQUE(reference, container_id)`**——远端节点的镜像 ID、磁盘占用对 Hub 没有意义（Hub 拿它做不了任何事，只能转发），硬塞进 `images` 会把「本机监控项」的语义搅浑，还要重建表改唯一约束。

```sql
CREATE TABLE nodes (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT    NOT NULL,              -- 用户起的名字，如「群晖」「香港VPS」
    token_hash    TEXT    NOT NULL,              -- 节点令牌的 sha256，明文只在生成时展示一次
    hub_url       TEXT    NOT NULL DEFAULT '',   -- 生成安装命令时用户填的 Hub 地址（内网或公网）
    hostname      TEXT    NOT NULL DEFAULT '',
    os            TEXT    NOT NULL DEFAULT '',
    arch          TEXT    NOT NULL DEFAULT '',
    agent_version TEXT    NOT NULL DEFAULT '',
    protocol      INTEGER NOT NULL DEFAULT 1,
    status        TEXT    NOT NULL DEFAULT 'offline',  -- online / offline
    last_seen_at  INTEGER NOT NULL DEFAULT 0,
    note          TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL DEFAULT 0
);

-- 每次上报全量替换（事务内 DELETE + INSERT），不做增量 diff
CREATE TABLE node_containers (
    node_id        INTEGER NOT NULL,
    container_id   TEXT    NOT NULL,
    container_name TEXT    NOT NULL DEFAULT '',
    image_ref      TEXT    NOT NULL DEFAULT '',
    image_id       TEXT    NOT NULL DEFAULT '',
    local_tag      TEXT    NOT NULL DEFAULT '',
    local_digest   TEXT    NOT NULL DEFAULT '',
    remote_digest  TEXT    NOT NULL DEFAULT '',
    latest_tag     TEXT    NOT NULL DEFAULT '',
    aliases        TEXT    NOT NULL DEFAULT '',
    status         TEXT    NOT NULL DEFAULT '',
    mode           TEXT    NOT NULL DEFAULT '',
    ignored        INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT    NOT NULL DEFAULT '',
    checked_at     INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX idx_node_containers ON node_containers(node_id, container_id);
```

`notifications` 和 `tasks` 各补两列（走现有 `ALTER TABLE` 补列机制）：

```sql
node_id   INTEGER NOT NULL DEFAULT 0   -- 0 = Hub 本机
node_name TEXT    NOT NULL DEFAULT ''
```

`node_id` 默认 0 的含义正好和「本机」重合，老库升级后所有历史通知自动归到本机，不需要回填。

**权威源约定**：远端节点的真实状态以 Agent 上报为准，Hub 的 `node_containers` 是只读快照。Hub 上对远端项做「忽略 / 改检测模式」时，先下发指令给 Agent，Agent 改完下一轮上报自然反映；Hub 不自行改快照。这条不变量要守住，否则双写一定打架。

## 六、通信协议

WebSocket，`wss://<hub>/api/agent/ws`，Agent 主动连出。复用现有的 `gorilla/websocket`。

### 握手与鉴权

```
GET /api/agent/ws
Authorization: Bearer <node_token>
```

- 令牌 16 字节随机数的 hex 串（32 个字符，128 位熵），Hub 只存 sha256，比对用 `subtle.ConstantTimeCompare`（沿用 `push_token` 的做法）。
- 令牌绑定单个节点，可在页面单独吊销。
- 连接建立后 Hub 立刻回 `hello_ok`，带上 `node_id` 和当前 Hub 版本；Agent 据此确认注册成功。

### 帧格式

统一信封，`type` 决定 `data` 的结构：

```json
{ "type": "report", "seq": 12, "data": { } }
{ "type": "cmd",    "id": "c-7f3a", "action": "update", "data": { } }
{ "type": "cmd_ack","id": "c-7f3a", "ok": true,  "data": { } }
```

| 方向 | type | 说明 |
|---|---|---|
| Agent → Hub | `hello` | 注册：主机名、系统、架构、版本、协议版本 |
| Agent → Hub | `report` | 巡检结果：容器快照数组（全量）+ 最近日志 |
| Agent → Hub | `task_progress` | 更新过程中的日志行，用于页面实时显示 |
| Agent → Hub | `cmd_ack` | 指令执行结果 |
| Agent → Hub | `pong` | 心跳回应 |
| Hub → Agent | `hello_ok` | 接受注册，返回 node_id |
| Hub → Agent | `cmd` | `scan` / `update` / `ignore` / `snooze` / `mode` / `self_upgrade` |
| Hub → Agent | `ping` | 心跳，30 秒一次 |

- 心跳由 Hub 发起，用 **Hub 的时钟**判定在线，不信 Agent 的本地时间。
- Agent 断线后指数退避重连，重连成功立即全量补报一次。
- 连续 3 个心跳周期没响应 → 标 `offline`；恢复即标 `online`，可选推一条「节点恢复」通知。

### 指令白名单

| action | 参数 | 作用 |
|---|---|---|
| `scan` | `force` | 立即巡检一轮 |
| `update` | `container_id`, `target_ref` | 重建指定容器 |
| `ignore` | `container_id`, `digest` | 忽略某个版本 |
| `snooze` | `container_id` | 稍后提醒 |
| `mode` | `container_id`, `mode` | 改检测模式 |
| `self_upgrade` | — | 用 helper 容器替换自己 |

指令带 `id`，Agent 执行完回 `cmd_ack`；Hub 侧 60 秒没收到 ack 就判失败并在卡片上标「节点无响应」。

## 七、Hub 侧交互

### 节点管理页

- 节点卡片：状态灯、主机名、安装时填的 Hub 地址、系统、Agent 版本、最后心跳。
- 「添加节点」：填名字 + **Hub 访问地址（必填）** → 生成令牌 → 直接给出可复制的 `curl | sh` 命令（地址、令牌都已内嵌）。
  - 地址框预填当前访问 Hub 的地址，可改。
  - 为空或不是 `http(s)://` 开头时，「生成」按钮禁用，并提示要填完整地址。
- 每行操作：改名、吊销令牌、重新生成安装命令、看日志、升级、删除。

### 列表页

- 容器 / 镜像列表顶部加节点下拉：`全部` / `本机` / `群晖` / `香港VPS`，每行带节点标签。
- 通知卡片标题前缀节点名：`[群晖] qbittorrent 有新版本可用`。

### 卡片按钮不用改

现有按钮 key 是 `update:<notification_id>`，服务端拿到通知后看 `n.NodeID` 决定走本机 `updater.Run()` 还是 WS 下发。**卡片结构、解析逻辑一律不动**，这是这次设计里最省事的一处。

## 八、代码改动范围

| 位置 | 改动 |
|---|---|
| `cmd/server/main.go` | 按 `AUTODOCKER_MODE` 分支：`serve`（现状）/ `agent`（不启 HTTP，启 hub client + 定时扫描） |
| `internal/hub/`（新） | Agent 侧 WS 客户端：重连、心跳、上报、指令分发、日志环形缓冲 |
| `internal/hubserver/`（新） | Hub 侧 WS 端点、节点注册与鉴权、指令下发与结果回收 |
| `internal/api/` | `GET /api/agent/install.sh`（含 Hub 地址合法性校验）、节点 CRUD、日志查询、升级触发 |
| `internal/store/` | `nodes` / `node_containers` 两表 + 若干查询 |
| `internal/model/` | `Node`、`NodeContainer` 类型；`Notification` / `Task` 加 `NodeID` |
| `Dockerfile` | HEALTHCHECK 按模式分支（Agent 模式不探 HTTP 端口） |
| `web/` | 节点管理页、列表节点筛选、通知卡片节点前缀 |

`internal/scanner`、`internal/updater`、`internal/dockerx`、`internal/registry` **一行不改**。

## 九、分期交付

| 版本 | 内容 | 验收标准 |
|---|---|---|
| **v1.7.0** | 节点注册、WS 通道、全量上报、install.sh、节点管理页、列表能看见远端容器与状态 | 目标机粘一条命令，Hub 里出现节点，并能看到它的容器和「有更新」标记 |
| **v1.8.0** | 下发 update 指令、进度回传、卡片按钮路由、任务记录带节点、Agent 日志回看 | 点卡片按钮，远端容器真的重建成功，卡片状态跟着变 |
| **v1.9.0** | 批量更新、一键升级全部节点、离线告警、节点分组视图 | — |

先只读再接写。上报链路跑不通的时候，谈远端点按钮更新没有意义。

## 十、风险与要提前处理的坑

1. **Hub 必须被 Agent 访问到**。家里飞牛没有公网 IP 的话，Agent 连不上——这是架构前提，不是 bug。解法：Cloudflare Tunnel / frp / 公网 VPS 反代。文档要给三条具体路径，不能只写「配置好网络」。
2. **容器 ID 跨节点会撞吗**：Docker 生成 64 位随机 hex，跨机重复概率可忽略；且 Hub 的远端数据在独立表里带 `node_id`，不涉及唯一键冲突。
3. **Agent 版本漂移**：协议带 `protocol` 字段。Hub 不认的版本在页面上明确标「Agent 过旧，请升级」，不静默失败。
4. **Hub 是单点**：Hub 挂了就没人发通知。这台机器必须常开（飞牛正好合适）。可选缓解（v1.9）：Agent 侧加「Hub 失联超过 N 分钟，用本地渠道自己发一条」的开关。
5. **反向代理要支持 WebSocket**：用户很可能用 Nginx / Caddy 反代 Hub，文档必须写明 `Upgrade` / `Connection` 头。Agent 连不上时日志要直接提示这一点，别让人猜。
6. **`docker.sock` 等于 root**：见第四节末尾。
7. **上报体积**：一台 100 个容器的机器，快照 JSON 约 50 KB，每轮巡检（默认 6 小时）一次，可忽略。
8. **时钟不可信**：所有「多久没上报」的判断都用 Hub 收到消息的时刻。
9. **`self_upgrade` 的失败面**：helper 容器拉不到镜像 / 起不来 → 节点失联。所以默认关闭自动升级，且 helper 要在替换前先验证新镜像能启动。

## 十一、附录：FN Connect 能不能当通信通道

结论：**不能**。不是配置问题，是机制上过不去。

三条依据：

1. **飞牛对第三方应用走「统一网关」，而网关会校验登录态。**
   fpk 应用可以在入口配置里声明 `gatewaySocket` + `gatewayPrefix`（如 `/app/autodocker`），应用监听一个 Unix Socket，飞牛网关**校验当前登录用户之后**才把 `/app/autodocker/...` 的请求转进去。这意味着 Agent 的每个请求都得带着飞牛的浏览器会话 Cookie。Agent 是后台程序，没有浏览器、没有登录流程，这条路走不通。

2. **FN Connect 的第三方应用入口是「先登录飞牛，再跳转」。**
   开启远程访问后，商店应用确实能通过 `https://应用名.<FNID>.5ddd.com` 打开，但前提是已经登录飞牛管理页面。这是飞牛刻意的设计（不少应用没有登录态，且公网开放 Web 服务涉及备案），不是能绕的限制。

3. **中继只开 443，免费版还限速。**
   FN Connect 的中继只转发 443 端口的 HTTPS，其他端口直接被拒。即便能打通，长连接 WebSocket 走中继也会受带宽和空闲策略影响。

另外，当前 fpk 用的是**直连模式**（`service_port = 19999`，桌面入口拼 `http://<nas-ip>:19999`），根本没注册到统一网关，网关这条路连入口都没有。

**所以方案不依赖 FN Connect。** 要从外网访问 Hub，三条路，按推荐度排：

| 方式 | 适合 | 代价 |
|---|---|---|
| **Cloudflare Tunnel** | 域名已经在 CF（现成的） | 装个 `cloudflared`，免费、纯出站、不用开任何端口 |
| DDNS + 端口映射 | 有公网 IP 或 IPv6 | 要路由器权限；19999 别裸奔到公网，前面得挂 TLS |
| frp / 自建反代 | 有公网 VPS | 多一台机器要维护 |

不管走哪条，最终都是产出一个公网地址。添加节点时填进「Hub 访问地址」那一格，安装命令就按这条路生成——**地址怎么来是用户自己的事，程序只管用**。

反代必须支持 WebSocket：Cloudflare Tunnel 默认支持，Nginx 要手写 `Upgrade` / `Connection` 头，Caddy 自动处理。

## 十二、还需要你定的两件事

已定：**Hub 访问地址由用户填写，必填才能生成安装命令**；外网入口怎么来不归程序管。

1. **远端更新的口径**：沿用 Hub 的全局设置（需确认 / 自动更新 / 失败回滚），还是每个节点单独配？建议 v1 沿用全局，节点级覆盖以后再说。
2. **自动升级的默认值**：我建议默认关闭、Hub 上一键手动升级。你要是觉得节点多了手动点烦，可以默认开。

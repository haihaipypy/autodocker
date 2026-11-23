# 多节点监控方案（远程服务器 / NAS）

> 本文只记设计与取舍。**操作步骤、界面说明见 [节点使用手册](nodes.md)。**
>
> 状态：1.13.0 全部落地 —— 节点接入与上报、远程一键重建与进度回传、远端提醒、
> Agent 自升级、按 compose 项目批量更新（先全拉再重建、按 `depends_on` 排序）、
> 加速源下发、失联告警、compose 项目里的版本同步。
> 目标：一台主控统一监控多台服务器 / NAS 上的容器，更新提醒汇到同一处，卡片按钮仍能一键更新。
> 约束：被监控端做到「跑一条命令，此后再也不用管」。

## 实现进度

| 能力 | 状态 | 说明 |
|---|---|---|
| 节点增删改查、令牌签发与吊销 | ✅ | 令牌明文只展示一次，库里存 sha256 |
| `curl \| sh` 一键安装 | ✅ | 脚本由 Hub 吐出，地址内嵌，幂等（重跑即升级） |
| WebSocket 长连接、注册、心跳、重连 | ✅ | 30s 心跳，90s 无帧断线，指数退避 1s→60s |
| 全量上报容器快照 | ✅ | 每轮巡检后整份替换，不做增量 |
| 节点页展示（状态 / 系统 / 版本 / 容器数 / 待更新数） | ✅ | 点开可看该节点的容器列表；连上但还没交过快照时显示「巡检中…」，不显示 0 |
| 手动触发远端巡检 | ✅ | `POST /api/nodes/{id}/scan` |
| **远程一键重建容器** | ✅ 1.8.0 | `POST /api/nodes/{id}/update`，只传容器编号，拉哪个引用由 Agent 自己算 |
| **更新进度回传** | ✅ 1.8.0 | `event` 帧：start → progress\* → done/failed，卡片顶部一条横幅 |
| **远端更新推提醒** | ✅ 1.8.2 | Hub 收到快照后判定「这一版还没说过」，按节点维度去重后推所有渠道 |
| **卡片按钮按节点路由** | ✅ 1.8.2 | 通知记 `node_id`，点「更新」下指令到那台机器；进度回来写回卡片 |
| **更新历史分节点** | ✅ 1.8.2 | Hub 侧为远端更新建自己的任务行，列表里挂节点标签 |
| Agent 日志回看 | ✅ | 最近 120 行随上报送出（页面上还没做展示，日志在 `Report.Logs` 里） |
| **节点自升级** | ✅ 1.9.0 | `POST /api/nodes/{id}/upgrade`；Hub 升级后节点连上来时自动跟进，失败整套回滚 |
| **批量更新、离线告警、加速源下发、compose 版本同步** | ✅ 1.10 ~ 1.13 | 见「加速源下发」「失联告警」「compose 项目里的版本同步」各节 |

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
| 升级 | Hub 自动下发，节点自己换（见「五、Agent 自升级」）；也可在节点卡片上手动点「升级 Agent」 |
| 卸载 | `docker rm -f autodocker-agent && docker volume rm autodocker-agent-data` |

**不开端口这件事值得单独说**：NAS 上装东西最烦的就是端口冲突和防火墙规则。Agent 一个端口都不占，装完就等于不存在。

### 安装：一条命令

Hub 页面点「添加节点」→ 填名字 + **必填 Hub 访问地址** → 生成令牌 → 页面给出这条命令（地址和令牌都已内嵌）：

```bash
curl -fsSL https://hub.1day.vip/api/agent/install.sh | sh -s -- --token=<32位十六进制令牌>
```

脚本**不拉镜像**，而是从 Hub 取程序本体（`GET /api/agent/binary`，用节点令牌鉴权），挂进一个 alpine 容器里跑。这么做有两个直接好处：

- **版本永远一致**：Agent 跑的就是 Hub 这一份字节，Hub 是 1.7.5，装上去就是 1.7.5。不会出现「`latest` 指向旧版本、装完不工作」。
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
  haihaipypy/autodocker:1.7.5
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

## 五、Agent 自升级

Agent 的二进制是宿主机上的文件绑进容器的（`-v /var/lib/autodocker-agent/autodocker:/app/autodocker:ro`），
所以进程没法就地覆盖自己，也不能"重启一下就好"。这里借鉴 watchtower 的思路，用**一次性 helper 容器**
在旧进程之外完成替换：

```
Hub ──upgrade 指令──▶ Agent
                        │  1. 从 Hub 下载新二进制到 /data/autodocker.next
                        │  2. 把当前容器的 Config / HostConfig 写成现场快照
                        │  3. docker create 一次性 helper（挂 docker.sock + 数据卷 + 二进制目录）
                        │  4. 回执「已开始升级」
                        ▼
                     helper（跑的是新二进制自己）
                        5. stop 旧容器 → 改名 autodocker-agent-old
                        6. 换二进制（.new → 目标文件，旧的留 .prev）
                        7. 按现场快照建新容器 → start
                        8. 盯 8 秒：稳定则删旧容器；不稳则删新容器、恢复旧容器与旧二进制
                        9. 把结局写进 /data/upgrade-result.json 后退出（容器自动回收）
```

几个刻意的选择：

| 选择 | 原因 |
|---|---|
| helper 跑**新**二进制 | 旧二进制里没有这段逻辑。代价是更早的版本无法自升级 —— 只能重跑一次安装命令，从这一版起闭环 |
| 停旧容器再建新的 | 不停的话两个 Agent 会抢同一条节点连接，Hub 上看起来像疯狂上下线 |
| 先建新容器、确认稳定再删旧的 | 顺序反过来的话，新版起不来就两头都没有了 |
| 失败必须回滚二进制 | 只有容器回来而二进制还是新的，下次重启会再失败一次 |
| 结局写在数据卷里 | 升级期间没有活着的连接可以回传，只能等新进程起来时捎给 Hub |
| Hub 侧记「正在升级」+ 5 分钟 TTL | helper 若在换容器那一刻被打断，按钮不能永远灰着 |
| 同版本最多自动试两次 | 试两次还上不去多半是环境问题（磁盘、权限、拉不动镜像），再试只会反复重启节点 |

自动触发点在**节点连上来的那一刻**：那是版本信息最新、连接最稳的时刻，同时天然覆盖
"刚装好的节点"和"刚被升级过的节点"。手动入口是节点卡片上的「升级 Agent」按钮，两者走同一条指令。

## 六、数据模型

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
    compose_project TEXT   NOT NULL DEFAULT '',   -- 来自容器标签 com.docker.compose.project
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

### 提醒按 compose 项目合并

一个项目里的服务通常一起发版，一次推 6 条消息、点 6 次，手机上弹一屏。所以提醒的粒度不是容器，而是「这一轮里同一个项目的那批容器」。

- **分组依据**只有一条：容器标签 `com.docker.compose.project`。名字前缀不去猜——自定义过 `container_name` 的项目（`safeline-chaos` → `safeline`）一猜就错。没有这个标签的容器（`docker run` 起的）各自一条。
- **成员逐条记账**：`notification_members` 一行一个容器，带自己的版本键与结果。去重、结果写回、逐容器状态全看这张表，单容器提醒也写一行，代码里不区分两种形状。
- **一键全更新是串行的**：底层更新器本来就是单飞的，并发重建同一台机器上的容器会抢端口、抢容器名，出错还分不清是谁的。串行慢一点，但每一步结果确定；某个失败就停在那一个，后面的保持待更新，重试时已成功的跳过。
- **先全拉、再重建**：整批分两段。预拉阶段只把镜像拉下来（指令 `pull`，Agent 的 `Updater.Pull`，`Preload=true` 不动监控项状态），任何一个拉不到就整批停下 —— 此时**容器一个都没被碰过**，整个项目原样留在旧版本。全部到位后才进入重建阶段。先前是「拉一个建一个」，第一个成员拉到一半失败，前面已经换成新版、后面还是旧版，一个 compose 项目就此停在新旧混搭上：旧的回不去，新的互相起不来。预拉不刷新镜像状态也刻意为之 —— 提前把 `latest_tag` 清掉，后面的重建会拿旧 tag 去建，镜像拉对了容器还是旧的。
- **老 Agent 自动退化**：不认识 `pull` 的 Agent 会回「不支持的指令」，Hub 认这句话，整体跳过预拉、按 1.10.3 的方式逐个「拉+建」，不把整批判失败。
- **重建按 `depends_on` 排顺序**：快照顺序取的是容器创建时间/名字，与依赖无关——`safeline-tengine` 很可能排第一个，它起来时 `safeline-mgt` 正在重建，连不上就 502 或退出被判失败。1.11.0 起 Agent 额外上报 `com.docker.compose.service` 与 `com.docker.compose.depends_on` 两个标签，Hub 在**整批下发之前**做一次拓扑排序（`internal/composeorder`，稳定排序，同层保持原顺序），预拉与重建都按这个顺序走。depends_on 里写的是**服务名**而不是容器名，所以两个标签必须一起上报，只有容器名对不上。
  - 排不出依赖就原样返回：没写 `depends_on`、用 `docker run` 起的、老 Agent 没上报标签，三种情况都是保持快照顺序，不报错。
  - 依赖目标不在本批（只更新项目里的一部分）按「已满足」处理，不参与排序。
  - compose 里写出环（只有写错才会有）时，环上那几个按原顺序接在末尾，整批照常进行，不会卡死。
  - 排序只解决「谁先起来」。`depends_on` 不保证上游已就绪（mgt 启动后还要几十秒才 ready），那层由重建后的健康检查等待兜住，两者配合才完整。
- **Agent 需要上报项目名**：老 Agent 不带这个字段，Hub 收到空串就退化成一条一个，不会出错。升到支持的那一版后自动合并。

节点页的容器列表同样按项目合并：一行一个项目、一个「更新」按钮，点一次由 Hub 侧依次下发（`POST /api/nodes/{id}/update` 带 `compose_project`，服务端回快照里现捞成员，不信前端传的列表）。进度记在 Hub 内存里（`BatchState`：一共几个、跑到第几个、停在哪一个），随节点列表接口一起返回。串行的理由与提醒那边一样，而且这里更硬：Agent 的更新器带锁，并发下发第二个会直接被拒。

**权威源约定**：远端节点的真实状态以 Agent 上报为准，Hub 的 `node_containers` 是只读快照。Hub 上对远端项做「忽略 / 改检测模式」时，先下发指令给 Agent，Agent 改完下一轮上报自然反映；Hub 不自行改快照。这条不变量要守住，否则双写一定打架。

## 七、通信协议

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
| Agent → Hub | `event` | 耗时操作的中间状态（见下节「远端更新怎么跑」） |
| Agent → Hub | `cmd_ack` | 指令执行结果 |
| Agent → Hub | `pong` | 心跳回应 |
| Hub → Agent | `hello_ok` | 接受注册，返回 node_id |
| Hub → Agent | `cmd` | 见下方白名单 |
| Hub → Agent | `ping` | 心跳，30 秒一次 |

- 心跳由 Hub 发起，用 **Hub 的时钟**判定在线，不信 Agent 的本地时间。
- Agent 断线后指数退避重连，重连成功立即全量补报一次。
- 连续 3 个心跳周期没响应 → 标 `offline`；恢复即标 `online`，可选推一条「节点恢复」通知。

### 指令白名单

| action | 参数 | 作用 | 状态 |
|---|---|---|---|
| `scan` | `force` | 立即巡检一轮 | ✅ |
| `update` | `container_id`, `container_name` | 重建指定容器 | ✅ 1.8.0 |
| `ignore` | `container_id`, `digest` | 忽略某个版本 | 未接 |
| `snooze` | `container_id` | 稍后提醒 | 未接 |
| `mode` | `container_id`, `mode` | 改检测模式 | 未接 |
| `upgrade` | `target_version`, `image_ref` | 用 helper 容器把自己换成 Hub 这一版（见第五节） | ✅ 1.9.0 |

不在白名单里的 action 一律回 `cmd_ack` 失败，没有任何「执行任意命令」的入口。

### 远端更新怎么跑

**只传容器编号，不传目标镜像。** 拉哪个引用由 Agent 用自己的巡检结果推导：仓库出了更高 tag 就换成新 tag，同 tag 换内容就拉原引用。Hub 手上那份快照本来就来自 Agent，再算一遍只是多一处可能算错的地方。

回执与进度是**分开**的：

1. Hub 下发 `cmd{action:"update"}`，**等一条 `cmd_ack`**（8 秒超时）；
2. Agent 校验完参数立刻 ack `ok`，然后才开始动手 —— 拉镜像要几十秒，回执不能等它跑完；
3. 之后每一步通过 `event` 帧回传：

```json
{ "type": "event", "data": {
  "op_id": "u-1732000000000000000", "kind": "update",
  "phase": "progress",           // start | progress | done | failed
  "container_id": "c1", "container_name": "cloudsaver",
  "message": "拉取镜像 cloudsaver/cloudsaver:latest",
  "task_id": 9, "at": 1732000000 } }
```

4. 结束（`done` / `failed`）后 Agent 补报一次全量快照，行状态跟着翻新。

这样设计的理由：等 ack 是为了让页面能当场说出「节点不在线」「Agent 版本太旧」；不等执行结果是因为那要几分钟，接口挂不住。

Hub 侧把这些事件拼成一份内存状态（`ops`），随 `/api/nodes` 一起返回，页面据此画横幅。状态**不落库** —— 它是「此刻在干什么」，进程重启就该丢掉，落库反而要处理一堆永远停在「更新中」的残留。

两个超时：完成态保留 90 秒（让页面至少读到一次结果），进行中态 5 分钟没有新事件就丢弃（Agent 崩了不会再有帧）。

`update` 用**脱离连接**的上下文执行：重建过程中 WS 断了重连，更新照跑完。断线只影响进度能不能送出去，不影响容器要不要重建 —— 否则会留下「旧容器已重命名、新容器还没建」的半截状态。

## 八、Hub 侧交互

### 节点管理页

- 节点卡片：状态灯、主机名、安装时填的 Hub 地址、系统、Agent 版本、最后心跳。
- 「添加节点」：填名字 + **Hub 访问地址（必填）** → 生成令牌 → 直接给出可复制的 `curl | sh` 命令（地址、令牌都已内嵌）。
  - 地址框预填当前访问 Hub 的地址，可改。
  - 为空或不是 `http(s)://` 开头时，「生成」按钮禁用，并提示要填完整地址。
- 每行操作：改名、吊销令牌、重新生成安装命令、看日志、升级、删除。

### 列表页

- 容器 / 镜像列表顶部加节点下拉：`全部` / `本机` / `群晖` / `香港VPS`，每行带节点标签。
- 通知标题前缀节点名：`1panel · cloud-saver 有新版本可用`。手机上一次收几条提醒时，
  只写容器名根本分不清是哪台机器上的那个容器。
- 更新历史里，远端任务在容器名后面挂一个节点标签；本机任务不加标签（否则满屏都是「本机」）。

### 卡片按钮不用改

现有按钮 key 是 `update:<notification_id>`，服务端拿到通知后看 `n.NodeID` 决定走本机 `updater.Run()` 还是 WS 下发。**卡片结构、解析逻辑一律不动**，这是这次设计里最省事的一处。

### 加速源下发（1.12.0）

节点的检测是 Agent 自己向注册表发 HTTP 请求，不经过 Docker 守护进程，所以那台机器
`daemon.json` 里的 `registry-mirrors` 对它无效。以前节点只能吃自己内置的两个默认源，
Hub 设置页填的地址管不到它 —— 表现就是宿主机拉得动、节点页整列「所有镜像源均失败」。

链路：

1. Hub 设置页开关 `node_mirror_follow`（默认开）→ `api.Server.NodeMirrors()` 返回三态：
   - `nil`：开关关着，什么都不下发；
   - `[]`：开关开着但地址留空，节点回到自己的配置；
   - `[a,b]`：整体替换，不合并不追加（顺序就是回退优先级，合并后无法解释）。
2. 两个下发时机：
   - **握手**：`HelloOK.Mirrors` 随注册回执一起给，连上那一刻就生效；
   - **保存设置**：`hubserver.BroadcastConfig()` 给所有在线连接发 `ActionConfig`，
     Agent 收到后 `registry.Client.SetOptions()` 热更新，并在下次巡检生效。
3. Agent 侧落进设置表 `agent_mirrors`，重启后先用这份、连上 Hub 再覆盖 ——
   Hub 一时连不上时检测不会退回内置默认。
4. 老 Agent 回「不支持的指令: config」，Hub 只记 Debug：这是纯增量能力，
   不该因为某个节点落后就把链路当成故障。

`CmdAck` 现在会带回 `action`：Hub 之前分不清失败的是哪条指令，
一条「不支持的指令」和真正的更新失败混在一起刷 Warn。

### 镜像 ID 起的容器（1.12.1）

容器的 `Config.Image` 不一定存着仓库引用。`docker run <镜像ID>` 起的、或在面板里按 ID
重建过的容器，存的是一串十六进制 ID。这串东西喂给 `ParseReference` 照样能解析
（`library/c4d252255b7b:latest`），语法过得去、语义是假的：查注册表必然「不存在」，
而用户看到的是「所有镜像源均失败」，去换源、查网络，全都不对症。

采集时先还原：

1. `registry.IsImageID()` 认出裸十六进制（12~64 位，可带 `sha256:` 前缀）。
   带斜杠或冒号的值一律按引用处理 —— 误判成 ID 会让正常镜像失去检测能力。
2. 用镜像 ID 问 `GET /images/{id}/json`，取 `RepoTags`。挑法固定：
   Docker Hub 的排在其它注册表前面，带版本号的排在 `latest` 前面，其余按名字排。
   规则固定下来，每轮挑中的才是同一个，否则「当前版本」那栏会来回跳。
3. 老记录是按那串 ID 存的，唯一键是「引用 + 容器号」，引用一变就是新键 ——
   所以先用 `UpdateImageAfterRebuild()` 就地改掉老记录，再照常检测。
   直接插新行会为同一个容器留下两条，用户的「忽略」「检测模式」也跟着丢。
4. 还原不了（`docker commit` 出来的镜像、标签已被删）：不查注册表，
   状态记 `unknown`，原因写「镜像只有 ID，本机找不到它对应的仓库名」。
   拿一个假引用去问，只会把用户永远钉在「检测异常」上。
5. 现场还有一种更常见的形态：`RepoTags` 与 `RepoDigests` **都为空** ——
   镜像是被同 tag 的新版本顶替掉的旧镜像，容器还跑着它，名字早已没了
   （safeline 的 `mgt` / `detector` 就是这样）。本地无从还原，但容器标签里有
   `com.docker.compose.service` 与 `com.docker.compose.project.working_dir`，
   于是提示语直接给 `docker compose up -d --force-recreate <服务名>`，
   让用户一次重建换回带 tag 的镜像，而不是让他去改一个页面上没有入口的字段。

### compose 项目里的版本同步（1.13.0）

compose 起的容器，镜像 tag 由 compose 文件（或它读的 `.env`）决定，而重建容器不经过 compose。
只换容器不改文件，下一次 `docker compose up -d` 就会把容器退回去 —— 这不是理论：
safeline 有一台从 9.4.7 掉回 9.4.3，就是三个服务被单独更新过、`.env` 里一直写着 9.4.3。

所以重建之前多一步：把那一处 tag 改到目标版本。

**怎么读宿主机上的文件。** 本程序跑在容器里，宿主机的 `/data/safeline` 在它这边根本不存在。
可行路径只有一个：创建一个临时容器（镜像用本程序自己的，本机一定已有；`Image` 传镜像编号，
不依赖某个 tag 还在不在），把 compose 文件所在目录与项目目录按原路径 bind 进去，再用
`GET/PUT /containers/{id}/archive` 读写文件 —— 这个接口的路径是在容器的挂载空间里解析的，
所以对 bind 进来的宿主机目录同样有效。写完立刻删掉临时容器。裸机部署（进程就在宿主机上）
则直接读盘，不绕这一圈。

**改哪里。** `image:` 的值拆成「仓库」与「tag」时要注意跳过 `${...}` 内部：`${IMAGE_TAG:?msg}`
里也有冒号，按「最后一个冒号」切会得到 `${IMAGE_TAG` 与 `?msg}`。tag 的两种形态：

- 变量（`${IMAGE_TAG}` / `${IMAGE_TAG:?提示}`）：改 `.env` 里那一行，引号风格保留；
- 字面量（`nginx:1.25.3`）：只改 `image:` 那一行的这个 token，其余逐字不动。

**三道核对，任何一条不满足就一个字都不改：**

1. 文件解析出来的引用必须与容器当前跑的引用一致。对不上一律拒绝 —— 那说明这个容器不是照
   这份文件建的，或者有人手改过，改文件等于赌一把。唯一例外：容器已经在目标版本上
   （上次更新成功但文件没跟上），补上正好。
2. 仓库名必须与目标一致，防止把 `nginx` 改成 `redis`。
3. tag 来源必须确定：变量要在 `.env` 里查得到；digest 固定、没写 tag、`${A}${B}` 拼出来的
   tag 一律不改。

**改不动也不拦更新。** 读写失败、`.env` 缺失、multiline 的 `image:`、权限不够 —— 都只在任务
日志里留一行「文件里的版本没跟着改，下次 compose up -d 会把版本退回去」，容器照常更新。
理由很直接：更新容器是主功能，改文件是补一致性；为了后者把前者拦住，等于让用户白点一次。

**回滚时把文件改回去。** 重建失败并回滚，说明容器又回到旧版本了，文件必须跟着回旧值，
否则下一次 compose up 会把「刚回滚掉的版本」重新升上去。改文件之前先写一份
`xxx.autodocker.bak` 给人看，撤销走的是内存里那份原文（备份文件不参与自动回滚）。

### 失联告警（1.12.0）

心跳与告警是两套机制，故意不合并：

| | 心跳 | 告警 |
|---|---|---|
| 判定 | 90 秒内没有任何帧 | `last_seen_at` 超过设定阈值（默认 10 分钟，下限 2 分钟） |
| 目的 | 页面立刻反映状态 | 决定要不要打扰用户 |
| 落点 | `nodes.status` | `nodes.offline_alert_at` + 推送 |

判断只看 `last_seen_at`，不看 `status`：status 是连接断开那一刻改的，进程卡住时可能
根本没改；时间戳是唯一不会骗人的依据。

四条防误报：

1. **启动宽限 3 分钟**：进程刚起来时所有节点都在重连，这段时间不判。
   注意 `MarkAllNodesOffline()` 仍然会在启动时执行（否则页面一片假在线），
   但它不再清 `offline_alert_at`。
2. **只认本次运行里连上过的节点**（内存 `seen`）：Hub 重启后一直没露面的节点
   说明不了「刚掉线」，只能是「一直没连上」。
3. **同一次失联只报一次**：判定命中立刻写 `offline_alert_at`（不等消息发出去，
   否则下一轮 30 秒后还会再挑中它一遍）；节点重连时清 0 并补一条「已恢复」。
4. **全体失联收敛成一条汇总**：本次见过的节点全部超时且不少于 2 台时，
   只说「N 个节点同时失联，先看 Hub 这台机器的网络」，不按台数刷屏。

通道走 `notify.Service.Push()`：纯文本、不建通知记录、不带按钮。
失联没有「哪个容器要更新」这回事，硬塞进 `Notification` 只会让历史列表多出点不动的条目。

## 九、代码改动范围

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

## 十、分期交付

| 版本 | 内容 | 验收标准 |
|---|---|---|
| **v1.7.0** | 节点注册、WS 通道、全量上报、install.sh、节点管理页、列表能看见远端容器与状态 | 目标机粘一条命令，Hub 里出现节点，并能看到它的容器和「有更新」标记 |
| **v1.7.5** | Agent 跳过镜像维度、快照唯一索引改普通索引（修「永远 0 个容器」）、巡检中状态 | 节点页数字是真的 |
| **v1.8.0** ✅ | 下发 `update` 指令、`event` 进度回传、`SendWait` 等回执、节点页「更新」按钮与横幅 | 点按钮，远端容器真的重建，过程实时可见 |
| **v1.8.2** ✅ | 远端提醒（去重键 + 节点名进标题）、卡片按钮按节点路由、进度写回卡片、任务记录带节点 | 手机上收到「1panel · cloud-saver 有新版本」，点更新就在那台机器上重建 |
| **v1.9.0** ✅ | 节点自升级：Hub 下发 `upgrade` 指令，Agent 用一次性 helper 完成替换与回滚；节点连上来时自动跟进版本 | Hub 升一版，两台节点自己跟上，卡片上的版本号变成新的 |
| **后续** | 批量更新、离线告警 | — |

先只读再接写。上报链路跑不通的时候，谈远端点按钮更新没有意义。

### 远端提醒是怎么做到不刷屏的

本机去重靠 `images.notified_tag` / `notified_digest` 两列，能把「这一版说过了」长期记住。
远端没有等价的落点 —— `node_containers` 每轮**整体替换**，任何写进快照的标记都活不过一次上报。

所以改记在通知自己身上：`notifications.version_key` 存「这一版的标识」，
有更高 tag 就是那个 tag，浮动 tag 就是远端摘要（各加前缀，防止 tag 恰好等于摘要短码时串味）。
新快照进来时逐条比对，命中就不再建通知。这同时解决了首轮上报「所有项都是新的」的问题：
首轮确实会为每个更新项各发一条，但那**就是**用户第一次装上 Hub 时该知道的事；
建完之后同一个 key 再也不会重复。

卡片上三个按钮的语义也随之对上：

| 按钮 | 效果 | 下一轮巡检 |
|---|---|---|
| 更新 | 下发指令到那台机器 | 容器已是最新，不再提醒 |
| 忽略 | 卡片标 ignored，记录留着 | 同一个 version_key 被挡住，不再提醒 |
| 稍后 | 卡片标 snoozed | 去重时**跳过** snoozed，下一轮重新推 |

推送本身放在独立协程里跑：企业微信长连接、钉钉建卡单条能等十几秒，
卡在 WS 读循环上等于把这台机器后续的所有上报和回执都堵在门口。

### 这一版刻意没做的

- **批量更新**：一次一台，避免同时重建多个容器把目标机打爆。
- **远端任务的重试**：Hub 只照 Agent 报的那一帧落结论，不重试也不猜。

## 十一、风险与要提前处理的坑

1. **Hub 必须被 Agent 访问到**。家里飞牛没有公网 IP 的话，Agent 连不上——这是架构前提，不是 bug。解法：Cloudflare Tunnel / frp / 公网 VPS 反代。文档要给三条具体路径，不能只写「配置好网络」。
2. **容器 ID 跨节点会撞吗**：Docker 生成 64 位随机 hex，跨机重复概率可忽略；且 Hub 的远端数据在独立表里带 `node_id`，不涉及唯一键冲突。
3. **Agent 版本漂移**：协议带 `protocol` 字段。Hub 不认的版本在页面上明确标「Agent 过旧，请升级」，不静默失败。
4. **Hub 是单点**：Hub 挂了就没人发通知。这台机器必须常开（飞牛正好合适）。可选缓解（v1.9）：Agent 侧加「Hub 失联超过 N 分钟，用本地渠道自己发一条」的开关。
5. **反向代理要支持 WebSocket**：用户很可能用 Nginx / Caddy 反代 Hub，文档必须写明 `Upgrade` / `Connection` 头。Agent 连不上时日志要直接提示这一点，别让人猜。
6. **`docker.sock` 等于 root**：见第四节末尾。
7. **上报体积**：一台 100 个容器的机器，快照 JSON 约 50 KB，每轮巡检（默认 6 小时）一次，可忽略。
8. **时钟不可信**：所有「多久没上报」的判断都用 Hub 收到消息的时刻。
9. **`self_upgrade` 的失败面**：helper 容器拉不到镜像 / 起不来 → 节点失联。所以默认关闭自动升级，且 helper 要在替换前先验证新镜像能启动。

## 十二、附录：FN Connect 能不能当通信通道

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

## 十三、还需要你定的两件事

已定：**Hub 访问地址由用户填写，必填才能生成安装命令**；外网入口怎么来不归程序管。

1. **远端更新的口径**：沿用 Hub 的全局设置（需确认 / 自动更新 / 失败回滚），还是每个节点单独配？建议 v1 沿用全局，节点级覆盖以后再说。
2. **自动升级的默认值**：我建议默认关闭、Hub 上一键手动升级。你要是觉得节点多了手动点烦，可以默认开。

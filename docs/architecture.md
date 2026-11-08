# 技术实现

> 返回 [README](../README.md)

## 目录结构

```
cmd/server/         程序入口与调度
internal/config     环境变量配置
internal/model      领域模型
internal/store      SQLite 持久化
internal/dockerx    Docker Engine API 封装
internal/registry   注册表 digest / tag 查询
internal/version    版本号解析与比较
internal/scanner    巡检编排（容器维度 + 镜像维度）
internal/updater    容器重建与回滚，以及只拉镜像的更新
internal/notify     通知路由（渠道选择、回调分发、通用投递记录）
internal/notify/feishu    飞书卡片与长连接回调
internal/notify/telegram  Telegram Bot API 客户端、消息与长轮询
internal/notify/dingtalk  钉钉互动卡片、accessToken 缓存与 Stream 长连接
internal/api        REST 接口
web/                前端单页（无需构建，go:embed 进二进制）

fnos/               飞牛 fnOS 应用包源码
  ├─ manifest       应用元数据（端口 19999 写死在这里）
  ├─ ICON*.PNG      应用中心图标
  ├─ app/bin/       编译出的二进制 + 启动包装脚本 run.sh
  ├─ app/ui/config  桌面入口定义（端口同样写死）
  ├─ cmd/           生命周期脚本（9 个：main / install / upgrade / uninstall / config）
  ├─ wizard/        安装、配置、卸载向导表单
  └─ config/        privilege（运行身份、docker 组）与 resource
tools/elfcheck      打包前校验 Linux 二进制是否完整
build-fpk.sh        一键打包成 .fpk
Dockerfile          多阶段构建（golang → alpine，纯静态二进制）
.dockerignore       排除 fnos / fpk / tools，只把源码送进构建上下文
.github/workflows/docker.yml  CI：双架构构建并推 GHCR
```

## 几处刻意的技术选择

**不用 Docker 官方 SDK。** `docker/docker@v27/v28` 依赖 `pkg/errors`，其 `go113.go`
只有旧式 `// +build go1.13` 约束，Go 1.26+ 已不识别 → `errors.Is/As` 未定义、整包编译失败。
`internal/dockerx` 直接封装 Engine REST API，`Config` / `HostConfig` 用 `map[string]any`
原样透传 —— 反而更适合「原样还原用户配置」这个需求，不会因为 SDK 结构体字段缺失而丢东西。

**SQLite 用 `modernc.org/sqlite`。** 纯 Go、免 CGO，交叉编译 arm64 只是改个环境变量。

**飞书用 WebSocket 长连接收回调。** 免公网、免备案、免反向代理；卡片必须是 2.0
（`schema: 2.0` + 按钮 `behaviors.callback`），旧版卡片的回传交互不支持长连接。

**Telegram 用 `getUpdates` 长轮询收回调。** 同样免公网；轮询与 webhook 互斥，
所以配过 webhook 的 bot 需先 `deleteWebhook`。群与频道 chat_id 是负数，
服务只能从「bot 最近收到过消息的会话」里得知 ID —— 这也是配置流程里必须先让它说一句话的原因。
两个渠道共用 `Notification.Channel` + `MessageID` 与回调状态，
加新渠道只需实现「发一条带按钮的消息」「改这条消息」「处理一次回调」三件事。

## 自检更新为什么只提醒、不自动装

程序每天去 GitHub 看一次自己有没有新版本，但**只把结果画在页面里 —— 不下载、不替换、不推送**。
这个限制是刻意划的，理由有五条：

- **更新本身不频繁。** 一年也就发几次版。为每年几次的操作写一套「下载 → 校验 → 替换」的
  链路，代码量和出错面都远大于它省下的那几下点击。
- **自更新失败是不可逆的。** 程序要替换的正是自己正在跑的那个二进制。下载到一半断网、
  sha256 对不上、替换时文件被占用、新版本起不来 —— 任意一步出错的结果都是「服务没了，
  而它本来是负责盯着别人服务的」。留着旧版本，至少还能打开页面看日志。
- **飞牛版不该自己动手。** fpk 装的程序归应用中心管，版本号也显示在应用中心里。程序私自
  把二进制换掉，应用中心的记录和实际文件就对不上，之后的升级、卸载、回滚全会错乱。
- **Docker 版受编排约束。** 端口、卷、网络、重启策略都在 compose 里，容器内的进程改不了
  自己的 `HostConfig`。真要更新只能在宿主机上 `pull` + `up -d`，所以给的是可直接粘贴的
  命令，而不是一个假的「一键升级」按钮。
- **不推 IM 是为了不吵。** 镜像更新的提醒才是主业，渠道里可能已经躺着几条；再塞一条
  「你自己有新版本了」，真正重要的那条最容易被划过去。检查结果留在页面，顶栏那颗胶囊
  已经够显眼。

代价是升级得用户自己动手一次。但装的到底是自己盯着的东西，知道它从哪来、装到哪去，
比一个黑盒替你做决定要稳。

## 通知渠道的分发与开关

**一条提醒广播到所有就绪渠道**，不是挑一个发。用户两个渠道都填好了却只收到一个，
第一反应是「另一个坏了」，所以 `readyChannels()` 返回候选全集，逐个投递，
一个失败不连累另一个，只有全部失败才返回 error。

`Notification.Deliveries []Delivery` 记全量投递坐标（`nid` / 渠道 / 会话 / 消息 ID），
`Channel` / `ChatRef` / `MessageID` 保留第一条作「主投递」，只用于列表展示。
**少记一条，那个渠道后续的卡片更新就找不到坐标**，消息会永远停在「待更新」。

**每个渠道一个总开关**（`feishu_enabled` / `telegram_enabled` / `dingtalk_enabled`）：

- 关掉只停「推新提醒」，配置项一个都不动 —— 用户的预期是「暂时静音」，不是「清空设置」。
- **已发出的卡片仍然继续回写状态**。`editDelivery` 刻意不看开关，只看凭据还在不在；
  否则用户中途关掉渠道，之前那条消息就永远停在「待更新」。
- 字段类型是 `*bool` 而非 `bool`：要区分「老库升级上来没这个字段」与「用户显式关闭」。
  为 nil 时按「凭据齐不齐」推导，否则升级那一刻用户会莫名静默。
- 设置页把「未配置」与「已暂停」分开表述：前者说「待配置」，后者说「已暂停：配置保留，
  不再推送新提醒」；顶栏状态胶囊同理，关掉全部渠道时写「通知已暂停」而不是「未连接」。
- 测通后自动打开开关（`enableChannelAfterTest`）—— 用户刚验证完链路，不会想再点一次开关。

## 钉钉的 Stream 与卡片

**企业内部应用 + 机器人 + Stream 长连接 + 互动卡片**。不用群机器人 webhook，
因为 webhook 只能单向推消息，收不到按钮点击，而「点按钮直接重建」是本项目的核心功能。

Stream 是官方反向 WebSocket：`POST /v1.0/gateway/connections/open` 拿到 `endpoint` + `ticket`
（90 秒有效、**只能用一次**），然后连 `wss://<endpoint>?ticket=<ticket>`。
同样免公网、免备案、免反代。几个必须按原样处理的点：

- 帧格式 `{specVersion, type, headers, data}`，`data` 是一层 JSON 字符串。
  ACK 通过**同一条 WebSocket 回写**（不是 HTTP），`headers.messageId` 必须与请求一致。
- `SYSTEM/ping` 要把 `data` **原样**回传（opaque，一字不差）；`SYSTEM/disconnect`
  是服务端负载均衡主动踢线，不响应，但必须用**新 ticket** 重连。
- 卡片回调的 `content` 是**一层转义的 JSON 字符串**，按钮参数在
  `content → cardPrivateData → params → action`，少解一层就永远取不到 action。
- `accessToken` 有效期约 7200 秒，必须缓存（提前 5 分钟刷新）；换 Client ID / Secret 时清缓存。
- `CreateAndDeliverCard` 的 `callbackType` 必须写 `"STREAM"`（大写），
  `openSpaceId = dtv1.card//IM_GROUP.<会话 ID>`，`imGroupOpenDeliverModel.robotCode` = Client ID。
  顶层 `success` 为真不代表投递成功，要逐个检查 `deliverResults[].success`。

**卡片模板在钉钉侧搭，代码只传数据。** 与飞书相反，钉钉不允许代码定义卡片结构：
必须先在卡片平台建模板、发布、拿 `cardTemplateId`，代码只传 `cardParamMap`。
变量名硬约定为 `title` / `status` / `body`（`dingtalk.VarTitle` 等），单值上限 1KB，
`clip()` 按字节截断但保证不切坏多字节字符。**更新卡片只改数据不改结构**，
所以按钮点完之后会留在卡片上（飞书/Telegram 换的是整条消息）。

**`outTrackId` 兼作通知 ID 的载体。** 飞书能把 `nid` 塞进按钮 `value`，
钉钉的按钮回传参数由模板定义、代码改不了，所以把 `nid` 编进卡片实例 ID：
`ad-<nid>-<纳秒>-<序号>`。序号是 `atomic.Uint64` —— 同一纳秒内 `time.Now().UnixNano()`
可能重复（Windows 时钟精度百纳秒级），撞了会报 `cardAlreadyExist`，
现象是「发了两条通知只收到一张卡」。

回调进来必须**反向校验**：`hasDingTalkDelivery(n, outTrackID)` 确认这条通知确实投递过这张卡。
ID 格式是可推导的，不校验等于「任何人构造一个 ID 就能让任意一条历史通知触发容器重建」
—— 而重建容器等同于系统最高权限。

**会话列表必须落盘。** 钉钉不提供「列出机器人所在群」的接口，会话 ID 只能靠
`/v1.0/im/bot/messages/get` 随消息带上来 —— 这份列表是不可再生的，只放内存的话
装个包重启一次就得让用户重新 @ 一次机器人。存 `settings` 表 `dingtalk_chats`（JSON），
不单开表：一条记录就 `{id,title,last_at}`，加表还得走 `ALTER COLUMN` 那套迁移。
`AttachChatStore` 时**合并而非覆盖**（本进程刚收的那份比磁盘上的新），
同一个群**标题跟着更新的那条走** —— 只要非空就覆盖，会让改过名的群一直显示旧名字。


**加速地址三层兜底：`用户设置 → 守护进程 registry-mirrors → 内置默认`。**
默认值刻意**不写进环境变量默认值**，否则中间那层永远进不去。
加速地址只作用于 Docker Hub —— 公共加速站基本只缓存 Hub，把 ghcr.io / quay.io
也改发过去只会换来一片 404。

## 企业微信：为什么只能补发一条消息

走官方长连接（`wss://openws.work.weixin.qq.com`）：连上 → `aibot_subscribe` 认证 →
每 30 秒 `ping` → 主动推送用 `aibot_send_msg`，按钮点击走 `aibot_event_callback`。
免公网、免加解密，与另外三个渠道的取态一致。但有两处必须先认清：

**1. 卡片只能在按钮事件后 5 秒内更新。**
`aibot_respond_update_msg` 必须带着**那次事件的 `req_id`** 发回去，服务端只认它；
而事件本身 5 秒内没有响应就被丢弃。重建容器要几十秒，这个窗口永远赶不上。
因此：点按钮时把卡片刷成「正在更新」（这一步在 5 秒内），更新结果**另发一条 markdown**，
原卡片就停在「正在更新」。卡片副标题里写明了「完成后会另发一条消息」——
不写的话，用户会以为卡住了。飞书 / Telegram / 钉钉都能事后改同一条消息，这里做不到，是协议限制。

**2. 卡片结构由代码定义，不用在后台搭模板。**
与钉钉相反（钉钉必须先在卡片平台建模板、变量名写错就一片空白）。代价是字段长度限制
要自己守：超长不会报错，只是被截断，所以 `clip()` 按**字符**而不是字节切 ——
按字节切中文会切出半个字，卡片上显示成方块。

几个实现上不得不然的点：

- **`task_id` 就是消息坐标。** 按钮回调只带 `task_id`（没有 message_id），
  所以它编进了通知 ID（`ad-<nid>-<纳秒>-<序号>`），回调靠它反查是哪条通知；
  并且必须核对「这个 `task_id` 确实在这条通知的投递记录里」——
  格式可推导，不校验等于任何构造一个 ID 就能触发任意历史通知重建容器。
- **单聊没有 `chatid`。** 消息与事件回调里 `chatid` 只在群聊出现，单聊要退到 `from.userid`，
  且主动推送时「接收会话」填的也是 userid。两处不统一，单聊用户就永远填不上会话。
- **读循环必须先于订阅启动。** 订阅的回执是从这条连接上读回来的；
  先 `request` 后起读循环，订阅会一直阻塞到超时 —— 表现为「连上了但什么都没发生」。
  同一类坑：连接对象要在拿到连接时立刻挂上，不能等订阅成功，
  否则服务端紧跟着推来的第一帧事件会因为「没有连接出口」被丢掉。
- **会话列表落盘**（`settings.wecom_chats`），理由与钉钉完全一样：没有反查群列表的接口。

## 重建容器的保真规则

这是出错代价最高的一段，改动前先看 `internal/updater/updater_test.go`。
照抄 `inspect` 结果反而会出错，所以要主动修正：

| 规则 | 为什么 |
| --- | --- |
| `Config.Image` **必须覆盖**成本次目标引用 | 照抄 inspect 会重建出仍跑着旧镜像的容器，「出现更高版本 tag」场景直接失效 |
| 自动分配的 `Hostname`（=旧容器短 ID）清掉，用户手写的 `--hostname` 保留 | 两个容器不能同名，新容器带着旧 container ID 会起不来 |
| 内置网络（`bridge`/`host`/`none`/`nat`）**不写进** `NetworkingConfig` | 写进去会被守护进程拒绝，是 host 网络容器更新失败最常见的原因 |
| 网络端点只搬 `IPAMConfig`/`Links`/`Aliases`/`DriverOpts` | `NetworkID`/`EndpointID`/`Gateway`/`IPAddress`/`MacAddress` 由运行时填充，原样回传会被当成「用户指定了 IP」而报地址冲突 |
| `container:` 网络模式**完全不传**端点 | 网络由被共享的容器决定，自己指定会冲突 |
| 匿名卷（镜像 `VOLUME` 声明、隐式创建的卷）**显式补回原卷名** | 否则数据会落到新空卷上 |

回滚链路是「停旧 → 改名 `<原名>-old-<时间戳>` → 建新 → 健康检查」，
所以回滚是有现场保障的，不是「删了重建」。

## 存储上容易踩的坑

- **`CREATE TABLE IF NOT EXISTS` 对已存在的表一个字段都不补** —— 加列必须走 `ALTER TABLE`
  （`store.imageAddedColumns` + `PRAGMA table_info`），否则 SELECT 报 `no such column`，
  页面整体白掉。
- `UpsertImage` 的 `ON CONFLICT DO UPDATE` **不能带上 `ignored` / `ignored_digest` /
  `mode` / `baseline_at`** —— 这些是用户侧设定，每轮扫描盖上等于没设。
- 异步任务用 `context.Background()`，不能用 handler 的 `r.Context()`（返回即撤销）。

## 前端约定

- **`display` 和 `hidden` 别混**：`style.css` 顶部有全局兜底
  `[hidden] { display: none !important; }`。曾因 `.login-mask { display: flex }` 压过它，
  表现为「登录成功却一直停在登录页」。新增浮层别删这条。
- 列宽用 `flex: 0 0 <px>` 固定，否则「当前 / 可用 / 状态」会逐行错位。
- `.btn.danger` 与 `.btn.ghost` 特异性相同 → 必须写 `.btn.danger:not(.ghost)`。
- **`data-theme` 必须在 `<head>` 内联脚本里同步写入**，不能等 `app.js` 加载后再改，
  否则深色主题会先闪一下白底。改这段脚本要保持同步执行、且排在样式表之后。
- **禁用 `color-mix()`**：飞牛桌面用内置 WebView 加载页面，Chromium 版本可能较旧；
  该函数不被支持时整条 `background` 声明会被判无效，主按钮直接掉成灰底。
  提亮用 `filter: brightness()`。
- `web/` 下新增资源**必须同步进 `web/embed.go` 的 `//go:embed` 列表**
  （显式文件名列表，漏了直接编译报错）。

## 图标资源

应用图标与网页图标是**同一张图的两种规格**，白底已抠成透明并做过边缘反混合
（源图是没有任何 alpha 通道的 RGB 白底图，直接放深色桌面上四个角会露出白边）：

| 文件 | 用途 |
| --- | --- |
| `fnos/ICON.PNG` / `fnos/ICON_256.PNG` | 应用中心与桌面入口图标（64 / 256） |
| `fnos/app/ui/images/icon_64.png` / `icon_256.png` | 与之同源，打包时一并放进 ui 目录 |
| `web/favicon.ico` | 浏览器标签页图标（16/32/48 三档） |
| `web/icon-192.png` | 登录页 logo（`<img src="/icon-192.png">`）与 apple-touch-icon |

- 抠底脚本在 `tools/icon-transparent.py`：靠 flood fill 从四角灌水定出形状（灌不到的就是内部的
  白色箭头），再按填充色反解 alpha。**注意 `Image.fromarray()` 返回的是只读图像，
  `ImageDraw.floodfill` 在它上面会静默失效**（不报错也不生效），必须先 `.copy()`。
- 改图后要跑 `tools/icon-scan.py` 复核四角 α 是否为 0，并用
  「合成回白底能否逐像素还原原图」来验证反混合是否干净。

## 测试

```bash
go test ./...
```

Requires Go 1.26+。覆盖的是**本地无法用眼睛验证、出错代价又最高**的那几处：

- `internal/updater` —— 重建配置的还原：镜像引用覆盖、Hostname 处理、匿名卷补齐、
  内置网络剔除、`-v` 表达式里容器内路径的解析；
- `internal/notify` —— 「点了更新按钮到底去拉哪个镜像」的判定；
  渠道开关的过滤语义；`outTrackId` 的编解码与唯一性（同一纳秒内不得重复）；
  会话列表写库再读回的往返（重启后仍可选）与坏数据容错；
- `internal/notify/feishu` —— 卡片 2.0 结构约束：`schema`、`body.elements`、
  按钮 `behaviors.callback` 及其 `value`；测试卡片的按钮回传载荷；
- `internal/notify/dingtalk` —— Stream 协议端到端（假网关回放 ping → 卡片回调 →
  机器人消息 → disconnect 重连，验证 opaque 原样回传、两层 JSON 解析、新 ticket 重连）；
  投放请求体的场域与 `callbackType`；accessToken 缓存与换凭据清缓存；
  卡片变量名与 1KB 截断；会话列表跨实例存活与合并（同 ID 取更新的标题与时间）；
- `internal/scanner` —— 通知标题与正文的措辞（含「不该出现内部术语」的反向断言）；
  以及「忽略本版本」的抑制边界：远端摘要没变就不推、变了照推；
- `internal/store` —— 首页统计的**口径**必须只数容器，本机镜像与纯远端项不能计入；
  「已忽略版本」的标记要能扛住下一轮扫描的 upsert、并在恢复监控时被清掉；
- `internal/version` —— 版本号比较，含「预发布版不能高于正式版」「tag 结尾的架构号
  （`v3-386` 的 `386`）不算版本段」这类容易写反的规则；
- `internal/registry` —— 镜像引用解析、tag 替换、Docker Hub 写法归一；
  **加速源路由**（只接管 Docker Hub、按序回退、多源失败原因聚合）；
  加速地址解析（协议头、结尾斜杠、多种分隔符、去重保序）；
  **回环地址自动走 http** 的判定（含「私有网段不该被误判」的反向断言）。

交叉编译：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o autodocker ./cmd/server
```

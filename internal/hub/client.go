// Package hub 实现 Agent 侧的长连接客户端。
//
// 方向永远是 Agent 主动连出：目标机不需要开放任何端口，也不用做端口映射。
// 断线后按指数退避重连，重连成功立刻补报一次全量快照 —— 断线期间产生的
// 变化不能靠「下轮再说」，否则用户会在 Hub 上看到一份过期的数据。
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/hubproto"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/registry"
	"github.com/haihaipypy/autodocker/internal/scanner"
	"github.com/haihaipypy/autodocker/internal/selfupgrade"
	"github.com/haihaipypy/autodocker/internal/store"
	"github.com/haihaipypy/autodocker/internal/updater"
	"github.com/haihaipypy/autodocker/internal/version"
)

// Config Agent 客户端配置。
type Config struct {
	HubURL       string // Hub 的访问地址，形如 https://hub.example.com 或 http://192.168.2.10:19999
	Token        string // 节点令牌（明文只在生成安装命令时出现一次）
	NodeName     string // 显示名，缺省用 hostname
	Version      string // Agent 版本，用于 Hub 判断是否落后
	ScanInterval time.Duration

	// 自身容器：自动升级时要停掉、重建的就是它。
	SelfContainer   string
	SelfContainerID string

	// 重连退避
	ReconnectMin time.Duration
	ReconnectMax time.Duration

	// BaseMirrors 是这台机器自己配的加速源（环境变量 / 安装时填的）。
	//
	// Hub 下发空列表表示「别用它那套了」，此时回退到这里 —— 不是回退到空，
	// 那样会把本来能用的配置一起抹掉。
	BaseMirrors []string

	// InitialMirrors 是启动那一刻实际该用的源。
	//
	// 与 BaseMirrors 分开是因为两者会不一样：上次 Hub 下发过一份，
	// 节点重启后应当先按它走（Hub 可能一时连不上），但「Hub 说清空」时
	// 仍要退回 BaseMirrors —— 那才是这台机器自己的配置。
	InitialMirrors []string

	// OnMirrors 应用一份新的加速源。为 nil 表示这台 Agent 不管加速源。
	OnMirrors func(mirrors []string)
}

// Client Agent 侧客户端。
type Client struct {
	cfg  Config
	sc   *scanner.Scanner
	st   *store.Store
	dk   *dockerx.Client
	up   *updater.Updater
	log  *slog.Logger
	ring *Ring

	// gorilla/websocket 不允许并发写，所有发送都必须过这把锁。
	writeMu sync.Mutex
	conn    *websocket.Conn

	// 最近一次巡检的错误，随上报一起带给 Hub。
	// 不填这个字段的话，一次 Docker 抽风就会让 Hub 收到「零个容器」，
	// 把上一轮的好数据整份覆盖掉 —— 页面上看起来像是容器全被删了。
	scanErrMu sync.Mutex
	scanErr   string

	// 上一次自动升级的结局，只在重启后的第一份上报里带出去一次。
	// 那一次上报本身就是「我还活着、现在是哪个版本」的证明，顺手把
	// 升级有没有成功一起告诉 Hub，用户不用去翻目标机的 docker logs。
	upgradeRes *hubproto.UpgradeResult

	// 当前生效的加速源。只用来判断「这份配置跟上一份一样吗」——
	// 一样就什么都不做，免得每次重连都白刷一遍日志与设置表。
	mirrorMu sync.Mutex
	mirrors  []string
}

// MirrorsSettingKey 是「Hub 下发的加速源」在设置表里的键名。
//
// 节点重启后先用它，连上 Hub 之后再被覆盖：Hub 暂时连不上时，
// 检测仍然按用户配好的源走，而不是莫名其妙退回内置默认。
const MirrorsSettingKey = "agent_mirrors"

func (c *Client) mirrorList() []string {
	c.mirrorMu.Lock()
	defer c.mirrorMu.Unlock()
	return append([]string(nil), c.mirrors...)
}

func (c *Client) setMirrorList(list []string) {
	c.mirrorMu.Lock()
	c.mirrors = append([]string(nil), list...)
	c.mirrorMu.Unlock()
}

// New 构造客户端。ring 提供最近日志，随每次上报一起送出去；
// 传 nil 时内部自建一个，只是没有外部引用、日志就只在本地。
func New(cfg Config, sc *scanner.Scanner, st *store.Store, dk *dockerx.Client,
	up *updater.Updater, logger *slog.Logger, ring *Ring) *Client {
	if cfg.ReconnectMin <= 0 {
		cfg.ReconnectMin = time.Second
	}
	if cfg.ReconnectMax <= 0 {
		cfg.ReconnectMax = 60 * time.Second
	}
	if cfg.ScanInterval <= 0 {
		cfg.ScanInterval = 6 * time.Hour
	}
	if ring == nil {
		ring = NewRing(200)
	}
	// 起始状态：优先用上次实际生效的那份，没有就是本机自己的。
	initial := cfg.InitialMirrors
	if len(initial) == 0 {
		initial = cfg.BaseMirrors
	}
	return &Client{
		cfg: cfg, sc: sc, st: st, dk: dk, up: up, log: logger, ring: ring,
		mirrors: append([]string(nil), initial...),
	}
}

// Ring 返回日志环形缓冲，交给 slog 的 MultiWriter 用，
// 这样每次上报都能捎上最近若干行日志。
func (c *Client) Ring() *Ring { return c.ring }

// SetUpgradeResult 记下上一次自动升级的结局，随下一份上报带给 Hub。
func (c *Client) SetUpgradeResult(res *hubproto.UpgradeResult) { c.upgradeRes = res }

// Run 一直跑到 ctx 结束。内部的断线重连不向上抛错 —— 目标机可能长时间断网，
// 那不是需要人工介入的故障。
func (c *Client) Run(ctx context.Context) error {
	backoff := c.cfg.ReconnectMin
	for {
		if ctx.Err() != nil {
			return nil
		}
		connected, err := c.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil && connected {
			// 正常关闭（通常是 Hub 侧主动踢），短暂等待后重连，不按失败处理
			backoff = c.cfg.ReconnectMin
		}
		c.log.Warn("与 Hub 的连接已断开，稍后重连",
			"hub", c.cfg.HubURL, "err", errText(err), "retry_in", backoff.String())

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < c.cfg.ReconnectMax {
			backoff *= 2
			if backoff > c.cfg.ReconnectMax {
				backoff = c.cfg.ReconnectMax
			}
		}
	}
}

// session 建立一次连接并跑到底，返回是否成功握手过。
func (c *Client) session(ctx context.Context) (bool, error) {
	target, err := c.wsURL()
	if err != nil {
		return false, err
	}

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+c.cfg.Token)

	dialer := &websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
	}
	conn, resp, err := dialer.DialContext(ctx, target, hdr)
	if err != nil {
		if resp != nil {
			return false, fmt.Errorf("连接被拒绝（HTTP %d）", resp.StatusCode)
		}
		return false, err
	}
	defer conn.Close()

	c.writeMu.Lock()
	c.conn = conn
	c.writeMu.Unlock()
	defer func() {
		c.writeMu.Lock()
		c.conn = nil
		c.writeMu.Unlock()
	}()

	// 注册
	hello := hubproto.Hello{
		Hostname:     hostname(),
		OS:           c.osName(),
		Arch:         runtime.GOARCH,
		AgentVersion: c.cfg.Version,
		Protocol:     hubproto.Protocol,
		NodeName:     c.cfg.NodeName,
	}
	if err := c.send(hubproto.TypeHello, "", hello); err != nil {
		return false, err
	}

	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var f hubproto.Frame
	if err := conn.ReadJSON(&f); err != nil {
		return false, fmt.Errorf("等待 Hub 回执超时: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})

	switch f.Type {
	case hubproto.TypeHelloOK:
		var ok hubproto.HelloOK
		_ = json.Unmarshal(f.Data, &ok)
		c.log.Info("已连接 Hub", "node_id", ok.NodeID, "node", ok.NodeName, "hub_version", ok.HubVersion)
		if ok.ScanInterval > 0 {
			c.cfg.ScanInterval = time.Duration(ok.ScanInterval) * time.Second
		}
		// 加速源随回执一起来：连上的那一刻就该用上，而不是等第一次巡检
		// 撞一墙 401 之后再想起要换。
		c.applyMirrors(ok.Mirrors)
	case hubproto.TypeFatal:
		return false, errors.New("Hub 拒绝连接: " + f.Error)
	default:
		return false, errors.New("Hub 返回了意外的握手帧: " + f.Type)
	}

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 2)
	go func() { errCh <- c.readLoop(sctx, conn) }()
	go func() { errCh <- c.scanLoop(sctx, conn) }()

	select {
	case <-ctx.Done():
		return true, nil
	case err := <-errCh:
		return true, err
	}
}

// readLoop 只负责读：心跳回应与指令分发。
//
// 与飞书/钉钉长连接那边踩过的坑一样 —— 读写必须分开。
// 若在这里同步执行扫描（可能几分钟），心跳就断了，Hub 会判定节点离线。
func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		var f hubproto.Frame
		if err := conn.ReadJSON(&f); err != nil {
			return err
		}
		switch f.Type {
		case hubproto.TypePing:
			if err := c.send(hubproto.TypePong, "", nil); err != nil {
				return err
			}
		case hubproto.TypeCmd:
			go c.handleCmd(ctx, f)
		case hubproto.TypeFatal:
			return errors.New("Hub 要求断开: " + f.Error)
		}
	}
}

// handleCmd 执行一条指令。这里就是「指令白名单」的落点：
// 不在 switch 里的 action 一律拒绝，没有任何「执行任意命令」的入口。
func (c *Client) handleCmd(ctx context.Context, f hubproto.Frame) {
	switch f.Action {
	case hubproto.ActionScan:
		var cmd hubproto.Cmd
		_ = json.Unmarshal(f.Data, &cmd)

		_, scanErr := c.sc.Scan(ctx, cmd.Force)
		if scanErr != nil && !errors.Is(scanErr, scanner.ErrScanRunning) {
			c.setScanErr(scanErr.Error())
		} else {
			c.setScanErr("")
		}

		// 巡检失败也要上报一次：Hub 靠这个错误知道「这台机器现在查不动」，
		// 同时因为 containers 是旧数据、ScanError 非空，它会保留既有快照，
		// 页面上不会因为一次 Docker 抖动就整台机器清空。
		if err := c.report(ctx); err != nil {
			c.ack(f, false, "上报失败: "+err.Error())
			return
		}
		if scanErr != nil && !errors.Is(scanErr, scanner.ErrScanRunning) {
			c.ack(f, false, scanErr.Error())
			return
		}
		c.ack(f, true, "巡检完成")

	case hubproto.ActionUpdate:
		var cmd hubproto.Cmd
		_ = json.Unmarshal(f.Data, &cmd)
		if cmd.ContainerID == "" {
			c.ack(f, false, "缺少容器标识")
			return
		}
		if c.up == nil {
			c.ack(f, false, "这台 Agent 未启用更新能力")
			return
		}
		if c.up.Busy() {
			c.ack(f, false, hubproto.ErrBusy)
			return
		}
		// 先回执再动手：重建要几十秒到几分钟，回执必须立刻发出去，
		// 否则 Hub 只能等到结束才知道这条指令到底有没有被接受。
		// 后续进度一律走 event 帧，回执只表示「收到并开工」。
		c.ack(f, true, "已开始更新")
		c.runUpdate(ctx, cmd)

	case hubproto.ActionPull:
		var cmd hubproto.Cmd
		_ = json.Unmarshal(f.Data, &cmd)
		if cmd.ContainerID == "" {
			c.ack(f, false, "缺少容器标识")
			return
		}
		if c.up == nil {
			c.ack(f, false, "这台 Agent 未启用更新能力")
			return
		}
		if c.up.Busy() {
			c.ack(f, false, hubproto.ErrBusy)
			return
		}
		c.ack(f, true, "开始拉取镜像")
		c.runPull(ctx, cmd)

	case hubproto.ActionUpgrade:
		var cmd hubproto.Cmd
		_ = json.Unmarshal(f.Data, &cmd)
		selfName := c.cfg.SelfContainer
		if selfName == "" {
			selfName = "autodocker-agent"
		}
		// 回执等准备做完再发：下载新二进制、拉新镜像都可能要几十秒，
		// 先回一句"已开始"然后失败，用户只会看到页面上的横幅转圈转到超时。
		// 反过来，准备阶段的失败能被如实带回去 —— 那才是能照做的错误信息。
		msg, err := selfupgrade.Prepare(ctx, c.dk, selfupgrade.Options{
			HubURL:        c.cfg.HubURL,
			Token:         c.cfg.Token,
			SelfContainer: selfName,
			SelfID:        c.cfg.SelfContainerID,
			CurrentVer:    c.cfg.Version,
			TargetVer:     cmd.TargetVersion,
			ImageRef:      cmd.ImageRef,
		}, c.log)
		if err != nil {
			c.ack(f, false, "无法自动升级: "+err.Error())
			return
		}
		c.ack(f, true, msg)
		// 从这里开始不再做任何事：几秒后 helper 会把本容器停掉。
		// 不回 event、不去刷新状态 —— 那些都要靠一条马上就不存在的连接。

	case hubproto.ActionConfig:
		var cmd hubproto.Cmd
		if err := json.Unmarshal(f.Data, &cmd); err != nil {
			c.ack(f, false, "配置内容无法解析: "+err.Error())
			return
		}
		if cmd.Mirrors == nil {
			c.ack(f, false, "配置里没有镜像加速地址")
			return
		}
		c.applyMirrors(cmd.Mirrors)
		if len(*cmd.Mirrors) == 0 {
			c.ack(f, true, "已改回本机自己的加速源")
			return
		}
		c.ack(f, true, fmt.Sprintf("已应用 %d 个加速地址", len(*cmd.Mirrors)))

	default:
		c.ack(f, false, "不支持的指令: "+f.Action)
	}
}

// applyMirrors 应用一份新的加速源并落盘。
//
// 落盘是为了「Hub 连不上的时候检测也别退化」：节点重启后、握手完成之前
// 用的是本地记住的那份，而不是内置默认。Hub 下发空列表时回退到本机配置，
// 把「用户清空了 Hub 那一格」和「把节点上的源也清干净」区分开。
func (c *Client) applyMirrors(m *[]string) {
	if m == nil {
		return
	}
	list := *m
	if len(list) == 0 {
		list = c.cfg.BaseMirrors
	}
	if slices.Equal(list, c.mirrorList()) {
		return
	}
	if c.cfg.OnMirrors != nil {
		c.cfg.OnMirrors(list)
	}
	c.setMirrorList(list)
	c.log.Info("已应用镜像加速地址", "mirrors", strings.Join(list, ","))

	// 存的是「实际在用的那份」，不是 Hub 原样下发的值 ——
	// 重启后要还原的是同一份行为，不是同一串字符。
	raw, err := json.Marshal(list)
	if err != nil {
		return
	}
	if err := c.st.SetSetting(MirrorsSettingKey, string(raw)); err != nil {
		c.log.Warn("记住加速地址失败", "err", err)
	}
}

// runUpdate 在目标机上重建一个容器，并把过程实时回传给 Hub。
//
// ctx 用的是**脱离连接**的上下文：重建过程中网络抖一下、WS 断开重连，
// 都不能让「旧容器已停、新容器还没建」这个窗口永远停在那里。
// 断线只影响进度能不能送出去，不影响更新本身要不要跑完。
func (c *Client) runUpdate(parent context.Context, cmd hubproto.Cmd) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 45*time.Minute)
	defer cancel()

	name := cmd.ContainerName
	imageRef, target := c.updateTarget(cmd.ContainerID, cmd.ContainerName)
	if name == "" {
		name = shortID(cmd.ContainerID)
	}
	if imageRef == "" {
		imageRef = cmd.ImageRef
	}

	taskID, err := c.st.CreateTask(&model.Task{
		ImageRef:      imageRef,
		ContainerID:   cmd.ContainerID,
		ContainerName: name,
		TriggeredBy:   "hub",
	})
	if err != nil {
		c.log.Error("创建更新任务失败", "container", name, "err", err)
		return
	}

	opID := fmt.Sprintf("u-%d", time.Now().UnixNano())
	emit := func(phase, msg string) {
		// 进度帧丢了不影响正确性：最终状态由更新结束后那次全量上报兜底。
		if err := c.send(hubproto.TypeEvent, "", hubproto.Event{
			OpID:          opID,
			Kind:          "update",
			Phase:         phase,
			ContainerID:   cmd.ContainerID,
			ContainerName: name,
			Message:       msg,
			TaskID:        taskID,
			At:            hubproto.Now(),
		}); err != nil {
			c.log.Debug("进度回传失败", "op", opID, "err", err)
		}
	}

	c.log.Info("收到更新指令", "container", name, "image", imageRef, "target", target, "task", taskID)
	emit(hubproto.PhaseStart, "开始更新 "+name)

	// 节流：拉镜像阶段 Docker 会刷出大量层进度，逐条转发会把 WS 打满，
	// 也会让 Hub 侧的状态更新抢在同一毫秒里。同一秒只发一条即可。
	var lastSent time.Time
	err = c.up.Run(ctx, updater.Request{
		TaskID:        taskID,
		ImageRef:      target,
		ContainerID:   cmd.ContainerID,
		ContainerName: name,
		TriggeredBy:   "hub",
		OnProgress: func(line string) {
			if time.Since(lastSent) < 900*time.Millisecond {
				return
			}
			lastSent = time.Now()
			emit(hubproto.PhaseProgress, line)
		},
	})

	if err != nil {
		c.log.Warn("远端更新失败", "container", name, "err", err)
		emit(hubproto.PhaseFailed, err.Error())
	} else {
		c.log.Info("远端更新完成", "container", name)
		emit(hubproto.PhaseDone, "更新完成")
	}

	// 无论成败都补一次全量上报：成功时行状态要变成「已是最新」，
	// 失败时容器可能已被回滚或停在半路，页面必须看到真实情况。
	if rerr := c.report(ctx); rerr != nil {
		c.log.Warn("更新后上报失败", "err", rerr)
	}
}

// runPull 只把镜像拉到目标机，不碰任何容器。
//
// 批量更新的第一步：整个项目的镜像先全部拉齐，任何一个拉不到就整批不动。
// 以前是「拉一个建一个」，第一个成员拉到一半失败时，前面的容器已经换成新版了，
// 一个 compose 项目就这样停在新旧混搭的状态上 —— 比不更新更难收拾。
func (c *Client) runPull(parent context.Context, cmd hubproto.Cmd) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 30*time.Minute)
	defer cancel()

	name := cmd.ContainerName
	imageRef, target := c.updateTarget(cmd.ContainerID, cmd.ContainerName)
	if name == "" {
		name = shortID(cmd.ContainerID)
	}
	if imageRef == "" {
		imageRef = cmd.ImageRef
	}
	if target == "" {
		// 本地库还没有这个容器的记录（刚创建、还没巡检到），拿 Hub 给的引用兜底。
		target = cmd.ImageRef
	}
	if target == "" {
		c.log.Warn("预拉缺少目标引用，已跳过", "container", name)
		return
	}

	taskID, err := c.st.CreateTask(&model.Task{
		ImageRef:      imageRef,
		ContainerID:   cmd.ContainerID,
		ContainerName: name,
		TriggeredBy:   "hub",
	})
	if err != nil {
		c.log.Error("创建拉取任务失败", "container", name, "err", err)
		return
	}

	opID := fmt.Sprintf("p-%d", time.Now().UnixNano())
	emit := func(phase, msg string) {
		if err := c.send(hubproto.TypeEvent, "", hubproto.Event{
			OpID:          opID,
			Kind:          "pull",
			Phase:         phase,
			ContainerID:   cmd.ContainerID,
			ContainerName: name,
			Message:       msg,
			TaskID:        taskID,
			At:            hubproto.Now(),
		}); err != nil {
			c.log.Debug("拉取进度回传失败", "op", opID, "err", err)
		}
	}

	c.log.Info("收到预拉指令", "container", name, "image", imageRef, "target", target, "task", taskID)
	emit(hubproto.PhaseStart, "开始拉取镜像 "+target)

	// 与更新走同一套节流：Docker 拉镜像时会按层刷大量进度行。
	var lastSent time.Time
	err = c.up.Pull(ctx, updater.PullRequest{
		TaskID:   taskID,
		ImageRef: target,
		Preload:  true,
		OnProgress: func(line string) {
			if time.Since(lastSent) < 900*time.Millisecond {
				return
			}
			lastSent = time.Now()
			emit(hubproto.PhaseProgress, line)
		},
	})
	if err != nil {
		c.log.Warn("预拉失败", "container", name, "err", err)
		emit(hubproto.PhaseFailed, err.Error())
		return
	}
	c.log.Info("预拉完成", "container", name, "target", target)
	emit(hubproto.PhaseDone, "镜像已就绪")
}

// updateTarget 从本地库推导这次要拉的目标引用。
//
// 判定留在 Agent 上 —— Hub 只转发「重建哪个容器」，不参与算版本。
// 升 tag 的场景（v3.6.0 → v3.7.0）要拉新 tag，同 tag 换内容的场景
// （latest 摘要变了）拉原引用。这个区别只有本地这份巡检结果知道。
//
// 编号查不到时按容器名再找一次：容器被重建过（compose 重建、外部升级）之后，
// Hub 手上的编号可能已经换成新的，而本地这份记录还没被下一轮巡检刷过来。
// 只认编号会推不出引用，最后拿空字符串去拉镜像 —— 报错还看不出是这一步。
func (c *Client) updateTarget(containerID, containerName string) (imageRef, target string) {
	img, err := c.st.FindImageByContainer(containerID)
	if (err != nil || img == nil) && containerName != "" {
		img, err = c.st.FindImageByContainerName(containerName)
	}
	if err != nil || img == nil {
		return "", ""
	}
	imageRef = img.Reference
	target = img.Reference
	if img.LatestTag == "" {
		return imageRef, target
	}
	ref, perr := registry.ParseReference(img.Reference)
	if perr != nil || version.Compare(img.LatestTag, ref.Tag) <= 0 {
		return imageRef, target
	}
	return imageRef, ref.WithTag(img.LatestTag)
}

// ack 回一条指令结果。
//
// 连 action 一起带回去：Hub 那边要靠它区分「这条失败要不要紧」。
// 比如老 Agent 收到不认识的 config 指令，回一句「不支持的指令」是正常现象，
// 不该和「更新失败」一样刷 Warn —— 没有 action，Hub 只能一刀切。
func (c *Client) ack(f hubproto.Frame, ok bool, msg string) {
	if f.ID == "" {
		return
	}
	af := hubproto.Frame{Type: hubproto.TypeCmdAck, ID: f.ID, Action: f.Action, OK: ok, Error: msg}
	if ok {
		af.Error = ""
	}
	_ = c.sendFrame(af, hubproto.CmdAck{Message: msg})
}

// scanLoop 周期巡检。连上后先立刻扫一次，让 Hub 马上有数据可看。
func (c *Client) scanLoop(ctx context.Context, conn *websocket.Conn) error {
	scan := func() {
		if _, err := c.sc.Scan(ctx, false); err != nil && !errors.Is(err, scanner.ErrScanRunning) {
			c.log.Warn("巡检失败", "err", err)
			c.setScanErr(err.Error())
			// 仍然上报：Hub 那边看到错误会保留上一轮快照，页面上不会突然空掉，
			// 但节点卡片上能看出这台机器现在查不动了。
			if rerr := c.report(ctx); rerr != nil {
				c.log.Warn("上报巡检失败状态失败", "err", rerr)
			}
			return
		}
		c.setScanErr("")
		if err := c.report(ctx); err != nil {
			c.log.Warn("上报巡检结果失败", "err", err)
		}
	}

	go scan()

	ticker := time.NewTicker(c.cfg.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			go scan()
		}
	}
}

// report 把本地库里的巡检结果整份推给 Hub。
//
// 数据来自 images 表而不是重新组织一遍：scanner 已经把状态、更高版本、
// 摘要这些东西算完并存好了，重新拼一次只会多一份可能算错的地方。
func (c *Client) report(ctx context.Context) error {
	images, err := c.st.ListImages(model.OriginContainer, model.OriginWatch, model.OriginLocal)
	if err != nil {
		return err
	}

	list := make([]hubproto.Container, 0, len(images))
	for _, img := range images {
		// 悬空镜像没有引用可比，Hub 那边也用不上
		if img.Dangling || img.Reference == "" {
			continue
		}
		// 镜像维度的「本机闲置镜像」不往 Hub 送：节点页只展示容器，
		// 这类条目送上去只会让快照里躺着一堆没人看、还拖慢上报的数据。
		if img.Origin == model.OriginLocal {
			continue
		}
		list = append(list, hubproto.Container{
			ContainerID:    img.ContainerID,
			ContainerName:  img.ContainerName,
			ComposeProject: img.ComposeProject,
			ComposeService: img.ComposeService,
			ComposeDeps:    img.ComposeDeps,
			ImageRef:       img.Reference,
			ImageID:        img.ImageID,
			LocalTag:       img.LocalTag,
			LocalDigest:    img.LocalDigest,
			RemoteDigest:   img.RemoteDigest,
			LatestTag:      img.LatestTag,
			Aliases:        img.Aliases,
			Origin:         string(img.Origin),
			Status:         string(img.Status),
			Mode:           string(img.Mode),
			Ignored:        img.Ignored,
			LastError:      img.LastError,
			CheckedAt:      checkedAt(img),
		})
	}

	rep := hubproto.Report{
		ScannedAt:  hubproto.Now(),
		Containers: list,
		Logs:       c.ring.Tail(120),
		ScanError:  c.lastScanErr(),
	}
	// 升级结局只带一次：带完就清掉，否则之后每轮上报都在说同一件旧事。
	if c.upgradeRes != nil {
		rep.Upgrade = c.upgradeRes
		c.upgradeRes = nil
	}
	if err := c.send(hubproto.TypeReport, "", rep); err != nil {
		return err
	}
	c.log.Info("已上报巡检结果", "containers", len(list))
	return nil
}

func (c *Client) setScanErr(msg string) {
	c.scanErrMu.Lock()
	c.scanErr = msg
	c.scanErrMu.Unlock()
}

func (c *Client) lastScanErr() string {
	c.scanErrMu.Lock()
	defer c.scanErrMu.Unlock()
	return c.scanErr
}

func checkedAt(img model.Image) int64 {
	if img.LastCheckedAt.IsZero() {
		return 0
	}
	return img.LastCheckedAt.Unix()
}

func (c *Client) send(typ, id string, data any) error {
	return c.sendFrame(hubproto.Frame{Type: typ, ID: id}, data)
}

func (c *Client) sendFrame(base hubproto.Frame, data any) error {
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		base.Data = raw
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.conn == nil {
		return errors.New("连接已断开")
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return c.conn.WriteJSON(base)
}

// wsURL 把 Hub 的 HTTP 地址换成 WS 地址。
//
// 用户填的可能是 http://、https://、带路径、带尾斜杠 —— 这里一次性收拾干净，
// 免得到处做字符串拼接。
func (c *Client) wsURL() (string, error) {
	raw := strings.TrimSpace(c.cfg.HubURL)
	if raw == "" {
		return "", errors.New("未配置 Hub 地址")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("Hub 地址不合法: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("Hub 地址必须以 http:// 或 https:// 开头: %s", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/agent/ws"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func hostname() string {
	h, err := osHostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

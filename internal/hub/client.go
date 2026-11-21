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
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/hubproto"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/scanner"
	"github.com/haihaipypy/autodocker/internal/store"
)

// Config Agent 客户端配置。
type Config struct {
	HubURL       string // Hub 的访问地址，形如 https://hub.example.com 或 http://192.168.2.10:19999
	Token        string // 节点令牌（明文只在生成安装命令时出现一次）
	NodeName     string // 显示名，缺省用 hostname
	Version      string // Agent 版本，用于 Hub 判断是否落后
	ScanInterval time.Duration

	// 重连退避
	ReconnectMin time.Duration
	ReconnectMax time.Duration
}

// Client Agent 侧客户端。
type Client struct {
	cfg  Config
	sc   *scanner.Scanner
	st   *store.Store
	dk   *dockerx.Client
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
}

// New 构造客户端。ring 提供最近日志，随每次上报一起送出去；
// 传 nil 时内部自建一个，只是没有外部引用、日志就只在本地。
func New(cfg Config, sc *scanner.Scanner, st *store.Store, dk *dockerx.Client, logger *slog.Logger, ring *Ring) *Client {
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
	return &Client{cfg: cfg, sc: sc, st: st, dk: dk, log: logger, ring: ring}
}

// Ring 返回日志环形缓冲，交给 slog 的 MultiWriter 用，
// 这样每次上报都能捎上最近若干行日志。
func (c *Client) Ring() *Ring { return c.ring }

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
			c.ack(f.ID, false, "上报失败: "+err.Error())
			return
		}
		if scanErr != nil && !errors.Is(scanErr, scanner.ErrScanRunning) {
			c.ack(f.ID, false, scanErr.Error())
			return
		}
		c.ack(f.ID, true, "巡检完成")
	default:
		c.ack(f.ID, false, "不支持的指令: "+f.Action)
	}
}

func (c *Client) ack(id string, ok bool, msg string) {
	if id == "" {
		return
	}
	f := hubproto.Frame{Type: hubproto.TypeCmdAck, ID: id, OK: ok, Error: msg}
	if ok {
		f.Error = ""
	}
	_ = c.sendFrame(f, hubproto.CmdAck{Message: msg})
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
			ContainerID:   img.ContainerID,
			ContainerName: img.ContainerName,
			ImageRef:      img.Reference,
			ImageID:       img.ImageID,
			LocalTag:      img.LocalTag,
			LocalDigest:   img.LocalDigest,
			RemoteDigest:  img.RemoteDigest,
			LatestTag:     img.LatestTag,
			Aliases:       img.Aliases,
			Origin:        string(img.Origin),
			Status:        string(img.Status),
			Mode:          string(img.Mode),
			Ignored:       img.Ignored,
			LastError:     img.LastError,
			CheckedAt:     checkedAt(img),
		})
	}

	rep := hubproto.Report{
		ScannedAt:  hubproto.Now(),
		Containers: list,
		Logs:       c.ring.Tail(120),
		ScanError:  c.lastScanErr(),
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

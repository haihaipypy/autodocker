package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// 企业微信智能机器人的长连接地址。
//
// 与钉钉要先用 HTTP 换 endpoint + ticket 不同，这里是固定地址：
// 连上之后发一条 aibot_subscribe 完成认证，之后收发都用同一条连接。
const wsEndpoint = "wss://openws.work.weixin.qq.com"

// 命令名，全部来自官方协议。
const (
	cmdSubscribe     = "aibot_subscribe"
	cmdPing          = "ping"
	cmdSendMsg       = "aibot_send_msg"
	cmdRespondUpdate = "aibot_respond_update_msg"

	cmdMsgCallback   = "aibot_msg_callback"
	cmdEventCallback = "aibot_event_callback"
)

// 事件类型。
const (
	eventTemplateCard = "template_card_event"
	eventDisconnected = "disconnected_event"
)

// maxChats 会话列表上限。
//
// 与钉钉同一个理由：这份列表只用来「点一下把会话 ID 填进设置」，
// 留 20 条足够，存太多只会让下拉框长得没边。
const maxChats = 20

// errReconnect 服务端主动断开（同一个机器人被新连接顶掉）。
//
// 这不是故障：企业微信规定一个机器人同一时间只能有一条长连接，
// 用户在别处再起一个实例就会把这条踢掉。按正常重连处理即可。
var errReconnect = errors.New("服务端断开长连接")

// CardEvent 一次模板卡片按钮点击。
type CardEvent struct {
	// TaskID 被点击卡片的 task_id，用来反查是哪条通知。
	//
	// 实测主动推送（aibot_send_msg）的卡片被点击时这里是空字符串，
	// 所以它只能当「优先路径」，不能当唯一路径。
	TaskID string
	// EventKey 被点按钮的原始 key，形如 update:12。
	EventKey string
	// Action 从 EventKey 拆出的动作（update / snooze / ignore）。空表示 key 不是本程序生成的。
	Action string
	// NotifID 从 EventKey 拆出的通知 ID：0 是测试卡片，正数是真实通知，-1 表示拆不出来。
	NotifID int64
	// ChatID 群聊会话 ID；单聊时为空。
	ChatID string
	// ChatType single / group。
	ChatType string
	// UserID 点按钮的人。
	UserID string
}

// Handler 业务侧要实现的回调。
//
// 方法名带 WeCom 前缀是必须的：同一个 Service 同时实现飞书、Telegram、
// 钉钉的处理器，Go 不允许同名方法。
type Handler interface {
	// HandleWeComCard 卡片按钮被点击。
	//
	// 返回值是要**立刻**写回卡片的内容，nil 表示不改。
	// 必须尽快返回：企业微信只给 5 秒，超时后这条回调被丢弃，
	// 卡片就停在原地 —— 用户看到的是「点了没反应」。
	HandleWeComCard(ev CardEvent) *TemplateCard
	// HandleWeComSession 机器人在某个会话里收到了消息，用来发现会话 ID。
	HandleWeComSession(chatID, chatType, userID string)
}

// ChatInfo 机器人最近收到过消息的会话。
type ChatInfo struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	ChatType string    `json:"chat_type"`
	LastAt   time.Time `json:"last_at"`
}

// ChatStore 会话列表的落盘位置。
//
// 企业微信没有任何接口能反查「这个机器人在哪些群里」，这份列表是靠收消息
// 攒出来的 —— 只放内存里的话，服务一重启（装包、改配置都会重启）就全丢，
// 用户得重新 @ 一次机器人。
type ChatStore interface {
	LoadChats() []ChatInfo
	SaveChats([]ChatInfo)
}

// Manager 管理与企业微信之间唯一的那条长连接，并承担全部发送。
//
// 与其它三个渠道不同，企业微信没有可用的 HTTP 发送接口：
// 主动推送（aibot_send_msg）和更新卡片（aibot_respond_update_msg）
// 都只能从这条 WebSocket 上发。所以「连接管理器」和「客户端」在这里合成了一个。
type Manager struct {
	baseCtx context.Context
	handler Handler
	log     *slog.Logger

	mu      sync.Mutex
	cfg     Config
	cancel  context.CancelFunc
	running bool
	lastErr string
	chats   []ChatInfo
	store   ChatStore
	gen     int

	// 当前连接的出口。连接建立后注入，断开时清空 ——
	// 发送方拿到 nil 就知道「现在发不出去」，不必自己判断连接状态。
	connMu sync.RWMutex
	conn   *conn

	// endpoint 连接地址，留空即官方地址。
	// 单测用一个本地 WebSocket 服务顶替，不必真的连企业微信。
	endpoint string
}

// New 构造管理器。
func New(baseCtx context.Context, h Handler, logger *slog.Logger) *Manager {
	return &Manager{baseCtx: baseCtx, handler: h, log: logger}
}

// endpointOr 实际连接地址。
func (m *Manager) endpointOr() string {
	if m.endpoint != "" {
		return m.endpoint
	}
	return wsEndpoint
}

// Config 当前配置。
func (m *Manager) Config() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// Configured 凭据是否齐全（能不能拉起长连接）。
func (m *Manager) Configured() bool { return m.Config().HasCredentials() }

// Ready 能不能真正发出一条消息：还得有接收会话。
func (m *Manager) Ready() bool {
	c := m.Config()
	return c.HasCredentials() && c.ChatID != ""
}

// MissingForSend 离「发得出去」还缺什么，供设置页显示。
func (m *Manager) MissingForSend() []string {
	c := m.Config()
	var out []string
	if c.BotID == "" {
		out = append(out, "BotID")
	}
	if c.Secret == "" {
		out = append(out, "Secret")
	}
	if c.ChatID == "" {
		out = append(out, "接收会话")
	}
	return out
}

// ChatID 配置里指定的接收会话。
func (m *Manager) ChatID() string { return m.Config().ChatID }

// Ensure 让长连接收敛到配置对应的状态。
//
// 只有 BotID / Secret 变了才重连：改接收会话、改开关都不需要 ——
// 重连会短暂丢掉按钮回调，而企业微信对 aibot_subscribe 有频率保护，
// 反复订阅可能触发系统限制。
func (m *Manager) Ensure(cfg Config) {
	cfg = cfg.Normalized()

	m.mu.Lock()
	defer m.mu.Unlock()

	same := m.cfg.BotID == cfg.BotID && m.cfg.Secret == cfg.Secret
	m.cfg = cfg
	if same && m.running {
		return
	}

	m.stopLocked()
	if !cfg.HasCredentials() {
		m.log.Info("企业微信 BotID / Secret 不完整，长连接保持关闭")
		return
	}

	m.gen++
	gen := m.gen
	ctx, cancel := context.WithCancel(m.baseCtx)
	m.cancel = cancel
	m.running = true
	m.lastErr = ""

	go func() {
		defer func() {
			if ctx.Err() == nil {
				m.mu.Lock()
				if m.gen == gen {
					m.running = false
				}
				m.mu.Unlock()
			}
		}()
		m.run(ctx, cfg)
	}()
}

// Stop 关闭长连接（进程退出前的收尾）。
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

func (m *Manager) stopLocked() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.running = false
	m.setConn(nil)
}

// Running 长连接的 goroutine 是否活着。
//
// 注意这只说明「在跑」，不代表「通了」—— 凭据写错时它照样在跑，只是不停重试。
// 判断真的通不通要结合 LastError。
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// LastError 最近一次建连失败的原因，空表示当前连接正常。
func (m *Manager) LastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

func (m *Manager) setLastErr(s string) {
	m.mu.Lock()
	m.lastErr = s
	m.mu.Unlock()
}

func (m *Manager) setConn(c *conn) {
	m.connMu.Lock()
	m.conn = c
	m.connMu.Unlock()
}

func (m *Manager) currentConn() *conn {
	m.connMu.RLock()
	defer m.connMu.RUnlock()
	return m.conn
}

// ---------- 会话列表 ----------

// Chats 机器人最近收到过消息的会话，按时间倒序。
func (m *Manager) Chats() []ChatInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ChatInfo, len(m.chats))
	copy(out, m.chats)
	return out
}

// AttachChatStore 接管会话列表的持久化，并把上次存的读回内存。
//
// 要在 Ensure 之前调用，否则服务重启后「读取会话」是空的，用户得再 @ 一次机器人。
func (m *Manager) AttachChatStore(cs ChatStore) {
	if cs == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = cs
	// 合并而非覆盖：本进程可能已经收过消息了，那份比磁盘上的新。
	m.chats = mergeChats(m.chats, cs.LoadChats())
}

// mergeChats 按 ID 去重，同一个会话取更晚的时间与非空的标题，最后倒序截断。
func mergeChats(a, b []ChatInfo) []ChatInfo {
	byID := make(map[string]int, len(a)+len(b))
	out := make([]ChatInfo, 0, len(a)+len(b))

	for _, c := range append(append([]ChatInfo{}, a...), b...) {
		if c.ID == "" {
			continue
		}
		if i, ok := byID[c.ID]; ok {
			if c.LastAt.After(out[i].LastAt) {
				out[i].LastAt = c.LastAt
				if c.Title != "" {
					out[i].Title = c.Title
				}
				if c.ChatType != "" {
					out[i].ChatType = c.ChatType
				}
			}
			continue
		}
		byID[c.ID] = len(out)
		out = append(out, c)
	}

	sortChats(out)
	if len(out) > maxChats {
		out = out[:maxChats]
	}
	return out
}

func sortChats(list []ChatInfo) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].LastAt.After(list[j-1].LastAt); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// rememberChat 记一条会话。
func (m *Manager) rememberChat(id, chatType string) {
	if id == "" {
		return
	}
	title := id
	switch chatType {
	case "group":
		title = "群聊 " + shortID(id)
	case "single":
		title = "单聊 " + id
	}

	m.mu.Lock()
	old := m.chats
	next := mergeChats([]ChatInfo{{ID: id, Title: title, ChatType: chatType, LastAt: time.Now()}}, old)
	m.chats = next
	store := m.store
	m.mu.Unlock()

	if store != nil {
		store.SaveChats(next)
	}
}

func shortID(s string) string {
	if len(s) > 10 {
		return s[:10] + "…"
	}
	return s
}

// ---------- 发送 ----------

// SendTemplateCard 向配置的会话主动推一张模板卡片。
func (m *Manager) SendTemplateCard(ctx context.Context, card TemplateCard) error {
	chatID := m.ChatID()
	if chatID == "" {
		return errors.New("尚未配置接收会话")
	}
	return m.sendBody(ctx, cmdSendMsg, map[string]any{
		"chatid":        chatID,
		"msgtype":       "template_card",
		"template_card": card.normalized(),
	}, 15*time.Second)
}

// SendMarkdown 向配置的会话主动推一条 markdown。
//
// 更新收尾时用的是它：企业微信不允许事后改卡片（见 BuildDoneMessage 的说明），
// 只能补发一条结果消息。
func (m *Manager) SendMarkdown(ctx context.Context, content string) error {
	chatID := m.ChatID()
	if chatID == "" {
		return errors.New("尚未配置接收会话")
	}
	return m.sendBody(ctx, cmdSendMsg, map[string]any{
		"chatid":   chatID,
		"msgtype":  "markdown",
		"markdown": map[string]string{"content": content},
	}, 15*time.Second)
}

// UpdateCard 用某个事件的 req_id 更新它对应的那张卡片。
//
// 必须在事件到达后 5 秒内调用，否则企业微信会丢弃这次响应 ——
// 它只认「同一个 req_id 的回写」，没有别的更新入口。
func (m *Manager) UpdateCard(ctx context.Context, reqID string, card TemplateCard) error {
	c := m.currentConn()
	if c == nil {
		return errors.New("企业微信长连接未建立")
	}
	return m.respondUpdate(ctx, c, reqID, card)
}

// respondUpdate 在指定连接上用事件的 req_id 回写卡片。
//
// 回执里的 errcode 必须检查：企业微信拒绝更新时依然会回一条正常帧，
// 只看「有没有收到回执」会把失败当成功，页面上显示已推送、卡片却纹丝不动。
func (m *Manager) respondUpdate(ctx context.Context, c *conn, reqID string, card TemplateCard) error {
	// 3 秒而不是 5 秒：官方只给 5 秒的更新窗口，等满就没意义了。
	a, err := c.request(ctx, cmdRespondUpdate, map[string]any{
		"response_type": "update_template_card",
		"template_card": card.normalized(),
	}, 3*time.Second, reqID)
	if err != nil {
		return err
	}
	if a.ErrCode != 0 {
		return fmt.Errorf("更新卡片被拒绝（errcode=%d %s）", a.ErrCode, a.ErrMsg)
	}
	return nil
}

// sendBody 从当前连接发一条命令并等回执。
func (m *Manager) sendBody(ctx context.Context, cmd string, body any, timeout time.Duration) error {
	c := m.currentConn()
	if c == nil {
		return errors.New("企业微信长连接未建立，无法发送（检查 BotID / Secret 与网络）")
	}
	a, err := c.request(ctx, cmd, body, timeout, "")
	if err != nil {
		return err
	}
	if a.ErrCode != 0 {
		return fmt.Errorf("发送被拒绝（errcode=%d %s）", a.ErrCode, a.ErrMsg)
	}
	return nil
}

// ---------- 连接生命周期 ----------

func (m *Manager) run(ctx context.Context, cfg Config) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}

		connected, err := m.connectOnce(ctx, cfg)
		if ctx.Err() != nil {
			return
		}
		if connected {
			// 连上过就从最短间隔重来：偶发一次断线不该接着用上次累积的等待。
			backoff = time.Second
		}
		if err != nil && !errors.Is(err, errReconnect) {
			m.setLastErr(err.Error())
			m.log.Warn("企业微信长连接中断，准备重连", "err", err, "retry_in", backoff.String())
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

// connectOnce 建一条连接、完成订阅、读到断开为止。
func (m *Manager) connectOnce(ctx context.Context, cfg Config) (connected bool, err error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		Proxy:            nil, // 企业微信走直连，不吃环境里的 http_proxy
	}
	raw, resp, derr := dialer.DialContext(ctx, m.endpointOr(), nil)
	if derr != nil {
		if resp != nil {
			return false, fmt.Errorf("WebSocket 握手失败（HTTP %d）: %w", resp.StatusCode, derr)
		}
		return false, fmt.Errorf("WebSocket 握手失败: %w", derr)
	}
	defer raw.Close()

	c := newConn(raw, m.log)

	// 连接对象一造出来就挂上去，**不能等订阅成功再挂**：
	// 服务端可能在回完订阅回执后立刻推第一帧事件，那一刻若还没有连接出口，
	// 按钮回调就会被当成「连接未建立」丢掉 —— 表现是「第一次点按钮永远没反应」。
	m.setConn(c)
	defer m.setConn(nil)

	// ctx 取消时主动关连接：ReadMessage 不认 context，不关就会一直卡住，
	// 表现为「停止后连接还在、下一轮又多一条」。
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = raw.Close()
		case <-done:
		}
	}()

	// 订阅即认证。失败（凭据错、频率限制）不重试太快 —— run 里的退避会兜住。
	// 读循环必须**先**跑起来：订阅的回执是从这条连接上读回来的，
	// 晚一步启动，订阅就会一直阻塞到超时（表现为「连上了但什么都没发生」）。
	go m.readLoop(ctx, c)

	ack, serr := c.request(ctx, cmdSubscribe, map[string]string{
		"bot_id": cfg.BotID,
		"secret": cfg.Secret,
	}, 15*time.Second, "")
	if serr != nil {
		return false, fmt.Errorf("订阅失败: %w", serr)
	}
	if ack.ErrCode != 0 {
		return false, fmt.Errorf("订阅被拒绝（errcode=%d %s），请核对 BotID 与 Secret", ack.ErrCode, ack.ErrMsg)
	}

	connected = true
	m.setLastErr("")
	m.log.Info("企业微信长连接已建立", "bot_id", cfg.BotID)

	// 30 秒一次心跳。失败就退出读循环，交给 run 重连 ——
	// 不回 ack 通常意味着连接已经半死，继续等下去只会一直「看起来在线」。
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go m.heartbeat(hbCtx, c)

	// 读到 ctx 结束或连接报错为止。
	select {
	case <-ctx.Done():
		return connected, nil
	case rerr := <-c.fatal:
		if ctx.Err() != nil {
			return connected, nil
		}
		return connected, rerr
	}
}

// heartbeat 每 30 秒 ping 一次。
func (m *Manager) heartbeat(ctx context.Context, c *conn) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ack, err := c.request(ctx, cmdPing, nil, 10*time.Second, "")
			if err != nil {
				m.log.Warn("企业微信心跳失败，准备重连", "err", err)
				c.closeWith(err)
				return
			}
			if ack.ErrCode != 0 {
				m.log.Warn("企业微信心跳返回错误", "errcode", ack.ErrCode, "errmsg", ack.ErrMsg)
			}
		}
	}
}

// readLoop 持续读帧：回执当场交还，业务帧丢给 dispatch 顺序处理。
//
// 业务帧**绝不能**在这里同步处理：更新卡片要等回执，而回执也只有这个循环读得到。
// 同步等就成了自己堵自己 —— 回执一直躺在 socket 里，5 秒一到报「超时」，
// 服务端到底改没改卡片也永远不知道（1.5.5 之前的表现）。
func (m *Manager) readLoop(ctx context.Context, c *conn) {
	frames := make(chan inFrame, 16)
	defer close(frames)
	go m.dispatch(ctx, frames)

	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			if ctx.Err() == nil {
				c.closeWith(fmt.Errorf("读取消息失败: %w", err))
			}
			return
		}

		var f inFrame
		if uerr := json.Unmarshal(data, &f); uerr != nil {
			m.log.Warn("企业微信推送不是合法 JSON，已跳过", "err", uerr)
			continue
		}

		// 有 errcode 的就是回执，按 req_id 交还给等待方。
		if f.ErrCode != nil {
			c.resolve(f.Headers.ReqID, ack{ErrCode: *f.ErrCode, ErrMsg: f.ErrMsg})
			continue
		}
		// 没有 cmd 也没有 errcode、只带 req_id 的帧同样是对某次请求的确认：
		// 不认它就只能干等到超时，把「已经成功」说成失败。
		if f.Cmd == "" && f.Headers.ReqID != "" && len(f.Body) == 0 {
			c.resolve(f.Headers.ReqID, ack{})
			continue
		}

		// 缓冲满了宁可丢帧也不阻塞读循环：卡住读循环等于连接假死。
		select {
		case frames <- f:
		default:
			m.log.Warn("企业微信回调处理不过来，已丢弃一帧", "cmd", f.Cmd)
		}
	}
}

// dispatch 按到达顺序处理业务帧。
//
// 单 goroutine 顺序处理：连点两下按钮也不会并发改同一条通知。
func (m *Manager) dispatch(ctx context.Context, frames <-chan inFrame) {
	for f := range frames {
		switch f.Cmd {
		case cmdMsgCallback:
			m.onMessage(f)
		case cmdEventCallback:
			m.onEvent(ctx, f)
		}
	}
}

// onMessage 处理用户发来的消息：只用来发现会话 ID。
func (m *Manager) onMessage(f inFrame) {
	var b struct {
		ChatID   string `json:"chatid"`
		ChatType string `json:"chattype"`
		From     struct {
			UserID string `json:"userid"`
		} `json:"from"`
	}
	if err := json.Unmarshal(f.Body, &b); err != nil {
		m.log.Warn("解析企业微信消息回调失败", "err", err)
		return
	}

	// 单聊回调不带 chatid，主动推送时又把「接收会话」填成 userid，
	// 所以这里要统一成同一个值，否则用户从单聊发完消息还是会话对不上。
	id := b.ChatID
	if id == "" {
		id = b.From.UserID
	}
	if id == "" {
		return
	}
	m.rememberChat(id, b.ChatType)
	if m.handler != nil {
		m.handler.HandleWeComSession(id, b.ChatType, b.From.UserID)
	}
}

// onEvent 处理事件回调。
//
// 目前只关心模板卡片按钮点击：它带着 req_id，必须在 5 秒内用这个 req_id
// 把卡片改掉，否则这次回调直接作废。
func (m *Manager) onEvent(ctx context.Context, f inFrame) {
	var b struct {
		ChatID   string `json:"chatid"`
		ChatType string `json:"chattype"`
		From     struct {
			UserID string `json:"userid"`
		} `json:"from"`
		Event struct {
			EventType string `json:"eventtype"`
			EventKey  string `json:"event_key"`
			TaskID    string `json:"task_id"`
			// 官方类型定义把按钮信息平铺在 event 下，但企业微信的字段层级
			// 不同版本并不完全一致，多认一层不会误伤：取不到就是零值。
			TemplateCardEvent struct {
				EventKey string `json:"event_key"`
				TaskID   string `json:"task_id"`
			} `json:"template_card_event"`
		} `json:"event"`
	}
	if err := json.Unmarshal(f.Body, &b); err != nil {
		m.log.Warn("解析企业微信事件回调失败", "err", err)
		return
	}
	if b.Event.EventKey == "" {
		b.Event.EventKey = b.Event.TemplateCardEvent.EventKey
	}
	if b.Event.TaskID == "" {
		b.Event.TaskID = b.Event.TemplateCardEvent.TaskID
	}

	switch b.Event.EventType {
	case eventDisconnected:
		m.log.Info("企业微信服务端断开长连接（同一个机器人被其它连接顶掉）")
		m.currentConn().closeWith(errReconnect)
		return
	case eventTemplateCard:
		// 群聊事件带 chatid，单聊不带 —— 与消息回调同一个处理方式。
		id := b.ChatID
		if id == "" {
			id = b.From.UserID
		}
		if id != "" {
			m.rememberChat(id, b.ChatType)
		}

		ev := CardEvent{
			TaskID:   b.Event.TaskID,
			EventKey: b.Event.EventKey,
			ChatID:   id,
			ChatType: b.ChatType,
			UserID:   b.From.UserID,
		}
		ev.Action, ev.NotifID = parseButtonKey(ev.EventKey)
		// task_id 为空是常态（主动推送的卡片就是这样），留一行日志便于事后核对：
		// 只写 event_key 与 task_id 两个值，不打印整帧 —— 帧里有用户 ID 和消息内容。
		m.log.Info("收到企业微信卡片事件",
			"event_key", ev.EventKey, "task_id", ev.TaskID, "action", ev.Action, "nid", ev.NotifID)
		if ev.EventKey == "" {
			// 连按钮 key 都没有，说明事件结构和预期不一致。
			// 这时把原始帧打出来（截断），否则只能靠猜 —— 用户再点几次也定位不了。
			raw := string(f.Body)
			if len(raw) > 600 {
				raw = raw[:600] + "…"
			}
			m.log.Warn("企业微信卡片事件没有 event_key，原始回调如下", "body", raw)
		}
		if m.handler == nil {
			return
		}
		card := m.handler.HandleWeComCard(ev)
		if card == nil {
			// 业务侧决定不改卡片。必须留一行日志：否则现象与「回调根本没进来」一样。
			m.log.Info("企业微信卡片事件未产生新卡片，跳过更新", "event_key", ev.EventKey)
			return
		}
		// 立刻回写，不等任何后续动作 —— 5 秒窗口就是这么紧。
		c := m.currentConn()
		if c == nil {
			return
		}
		if f.Headers.ReqID == "" {
			// 没有 req_id 就无从对齐要改哪张卡片，这次回写注定失败，说清楚别让人猜。
			m.log.Warn("企业微信卡片事件没有 req_id，无法回写卡片", "event_key", ev.EventKey, "task_id", ev.TaskID)
			return
		}
		if err := m.respondUpdate(ctx, c, f.Headers.ReqID, *card); err != nil {
			m.log.Warn("回写企业微信卡片失败", "event_key", ev.EventKey, "task_id", ev.TaskID, "err", err)
		} else {
			m.log.Info("企业微信卡片已更新", "event_key", ev.EventKey, "card_type", card.CardType)
		}
	}
}

// ---------- 底层连接 ----------

// ack 一条命令的回执。
type ack struct {
	ErrCode int
	ErrMsg  string
}

// inFrame 服务端下发的帧。errcode 用指针区分「回执」与「推送」。
type inFrame struct {
	Cmd     string `json:"cmd"`
	Headers struct {
		ReqID string `json:"req_id"`
	} `json:"headers"`
	Body    json.RawMessage `json:"body"`
	ErrCode *int            `json:"errcode"`
	ErrMsg  string          `json:"errmsg"`
}

// outFrame 上行帧。
type outFrame struct {
	Cmd     string            `json:"cmd"`
	Headers map[string]string `json:"headers"`
	Body    any               `json:"body,omitempty"`
}

// conn 一条 WebSocket 连接及其未完成的请求。
type conn struct {
	ws  *websocket.Conn
	log *slog.Logger

	wmu     sync.Mutex
	pmu     sync.Mutex
	pending map[string]chan ack

	closeOnce sync.Once
	fatal     chan error

	seq atomic.Uint64
}

func newConn(ws *websocket.Conn, logger *slog.Logger) *conn {
	return &conn{
		ws:      ws,
		log:     logger,
		pending: make(map[string]chan ack),
		fatal:   make(chan error, 1),
	}
}

// closeWith 记录致命错误并关闭连接，读循环与心跳都会因此退出。
func (c *conn) closeWith(err error) {
	c.closeOnce.Do(func() {
		select {
		case c.fatal <- err:
		default:
		}
		_ = c.ws.Close()
	})
}

// request 发一条命令并等回执。
//
// reqID 为空时自动生成；更新卡片必须传事件里的 req_id（企业微信只认那个）。
func (c *conn) request(ctx context.Context, cmd string, body any, timeout time.Duration, reqID string) (ack, error) {
	if reqID == "" {
		reqID = fmt.Sprintf("%s-%d-%d", cmd, time.Now().UnixNano(), c.seq.Add(1))
	}

	ch := make(chan ack, 1)
	c.pmu.Lock()
	c.pending[reqID] = ch
	c.pmu.Unlock()
	defer func() {
		c.pmu.Lock()
		delete(c.pending, reqID)
		c.pmu.Unlock()
	}()

	if err := c.write(outFrame{
		Cmd:     cmd,
		Headers: map[string]string{"req_id": reqID},
		Body:    body,
	}); err != nil {
		return ack{}, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case a := <-ch:
		return a, nil
	case <-ctx.Done():
		return ack{}, ctx.Err()
	case <-timer.C:
		return ack{}, fmt.Errorf("等待 %s 回执超时（%s）", cmd, timeout)
	}
}

func (c *conn) write(v outFrame) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	// 写超时：对端半死时 WriteJSON 会一直等，把调用方卡死，
	// 表现成「连接还在、消息发不出去」。
	_ = c.ws.SetWriteDeadline(time.Now().Add(20 * time.Second))
	return c.ws.WriteJSON(v)
}

// resolve 把一个回执交给等待方。
func (c *conn) resolve(reqID string, a ack) {
	c.pmu.Lock()
	ch, ok := c.pending[reqID]
	c.pmu.Unlock()
	if !ok {
		// 订阅 / 心跳之外的异步回执（比如服务端对某次推送的确认）到此为止。
		return
	}
	select {
	case ch <- a:
	default:
	}
}

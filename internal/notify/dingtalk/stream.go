package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// 两个回调 topic。
//
// 机器人消息那条是**必须**订阅的：钉钉不提供「列出机器人所在群」的接口，
// 会话 ID 只能等机器人在群里收到一条消息时随着回调一起带上来。
const (
	TopicBotMessage   = "/v1.0/im/bot/messages/get"
	TopicCardCallback = "/v1.0/card/instances/callback"
)

// 系统推送的两个 topic。
const (
	systemTopicPing       = "ping"
	systemTopicDisconnect = "disconnect"
)

// maxChats 会话列表上限。
//
// 这份列表的唯一用途是「点一下把会话 ID 填进设置」，留 20 个足够；
// 存太多只会让设置页那个下拉框长得没边。
const maxChats = 20

// errReconnect 服务端主动要求断开：不是故障，按正常重连处理。
//
// 钉钉会定时踢长连接做负载均衡，这是协议的一部分。若把它当错误记到
// LastError 上，设置页会周期性地闪一条「连接已断开」，用户会以为配错了。
var errReconnect = errors.New("服务端要求重连")

// Handler 业务侧实现的两个回调。
//
// 方法名带 DingTalk 前缀是必须的：同一个 Service 还要实现飞书与 Telegram
// 的处理器，Go 不允许同名方法，而飞书那边已经占用了 HandleCardAction。
//
// 两者都必须是**快速返回**：钉钉等 ACK，超时后要么重推要么算失败。
// 耗时动作（重建容器）必须在实现里自己丢到后台去。
type Handler interface {
	// HandleDingTalkCard 卡片按钮被点击。outTrackID 定位是哪张卡片，
	// action 是模板里配的回传参数（update / snooze / ignore / test）。
	//
	// 返回值是要**当场**写回卡片的变量，nil 表示不改卡片。
	// 走返回值而不是事后再调一次更新接口，是因为只有 ACK 能让卡片在点击的
	// 那一瞬间变样；事后调接口是几百毫秒之后的事，这段空窗期就是
	// 用户嘴里的「点了完全没反应」。
	HandleDingTalkCard(outTrackID, action string) map[string]string
	// HandleDingTalkMessage 机器人在某个会话里收到了一条消息，
	// 用来发现 conversationId（钉钉不提供群列表接口）。
	HandleDingTalkMessage(conversationID, title string)
}

// ChatInfo 机器人最近收到过消息的会话。
type ChatInfo struct {
	ID     string    `json:"id"`
	Title  string    `json:"title"`
	LastAt time.Time `json:"last_at"`
}

// ChatStore 会话列表的落盘位置。
//
// 会话 ID 只能靠「机器人在群里收到一条消息」带上来，钉钉没有任何接口能主动查
// 一个应用在哪些群里 —— 所以这份列表本身就是不可再生的：存在内存里的话，
// 服务一重启（装包、改配置都会重启）就全丢，用户得重新 @ 一次机器人。
//
// 做成接口而不是直接依赖 store：这个包不该知道数据库长什么样，
// 单测里塞一个内存实现也比重开一个库省事。
type ChatStore interface {
	LoadChats() []ChatInfo
	SaveChats([]ChatInfo)
}

// StreamManager 管理 Stream 长连接的生命周期。
//
// 与飞书的 LongConnManager 同一个理由：凭据是装好之后在网页里填的，
// 服务启动那一刻往往还没有；填完、改错都要能即时生效，不必重启。
type StreamManager struct {
	baseCtx context.Context
	handler Handler
	log     *slog.Logger

	mu      sync.Mutex
	cfg     Config
	cancel  context.CancelFunc
	running bool
	lastErr string
	chats   []ChatInfo
	// chats 的落盘位置，可以为 nil（只记内存）。
	chatStore ChatStore
	gen       int
}

// NewStreamManager 构造长连接管理器。
func NewStreamManager(baseCtx context.Context, h Handler, logger *slog.Logger) *StreamManager {
	return &StreamManager{baseCtx: baseCtx, handler: h, log: logger}
}

// Ensure 让长连接收敛到给定配置对应的状态。
//
// 只有应用凭据或 API 地址变了才重连：改卡片模板、改接收群、改开关
// 都不需要重连 —— 重连会短暂丢掉按钮回调，过于频繁还可能触发连接数限制。
func (m *StreamManager) Ensure(cfg Config) {
	cfg = cfg.Normalized()

	m.mu.Lock()
	defer m.mu.Unlock()

	same := m.cfg.ClientID == cfg.ClientID &&
		m.cfg.ClientSecret == cfg.ClientSecret &&
		m.cfg.APIBase == cfg.APIBase
	if same {
		m.cfg = cfg // 会话/模板可能变了，记住新值但不重连
		return
	}

	m.stopLocked()
	m.cfg = cfg

	if !cfg.HasCredentials() {
		m.log.Info("钉钉应用凭据不完整，Stream 长连接保持关闭")
		return
	}

	m.gen++
	ctx, cancel := context.WithCancel(m.baseCtx)
	m.cancel = cancel
	m.running = true
	m.lastErr = ""

	go func() {
		defer func() {
			// 主动停止时不改 running：那多半是紧接着换新连接，
			// 这里再置 false 会把后来者的运行状态误标成已停止。
			if ctx.Err() == nil {
				m.mu.Lock()
				m.running = false
				m.mu.Unlock()
			}
		}()
		m.run(ctx, cfg)
	}()
}

// Stop 关闭长连接（进程退出前的收尾）。
func (m *StreamManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

func (m *StreamManager) stopLocked() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.running = false
}

// Running 长连接的 goroutine 是否活着。
//
// 注意这只说明「在跑」，不代表「连得上」——凭据写错时它照样在跑，
// 只是一直在重试。要判断真的通不通得结合 LastError。
func (m *StreamManager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// LastError 最近一次建连失败的原因，空表示当前连接正常。
func (m *StreamManager) LastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

func (m *StreamManager) setLastErr(s string) {
	m.mu.Lock()
	m.lastErr = s
	m.mu.Unlock()
}

// Chats 机器人最近收到过消息的会话，按时间倒序。
func (m *StreamManager) Chats() []ChatInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ChatInfo, len(m.chats))
	copy(out, m.chats)
	return out
}

// AttachChatStore 接管会话列表的持久化，并把上次存的会话读回内存。
//
// 要在 Ensure 之前调用，这样服务一起来「读取会话」就有内容可选，
// 不必等用户先 @ 一次机器人。
//
// 传 nil 表示只想记在内存里（默认行为）。
func (m *StreamManager) AttachChatStore(cs ChatStore) {
	if cs == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	m.chatStore = cs
	// 合并而非覆盖：本进程可能已经收过消息了，那份比磁盘上的新。
	m.chats = mergeChats(m.chats, cs.LoadChats())
}

// mergeChats 按会话 ID 去重，同一个群取更晚的时间与非空的标题，最后按时间倒序截断。
func mergeChats(a, b []ChatInfo) []ChatInfo {
	byID := make(map[string]int, len(a)+len(b))
	out := make([]ChatInfo, 0, len(a)+len(b))

	for _, c := range append(append([]ChatInfo{}, a...), b...) {
		if c.ID == "" {
			continue
		}
		if i, ok := byID[c.ID]; ok {
			// 标题必须跟着「更新的那条」走，不能只要是非空就覆盖 ——
			// 否则磁盘上那条旧标题会把本进程刚收到的新标题盖回去
			// （群名改过之后，设置页会一直显示旧名字）。
			switch {
			case c.LastAt.After(out[i].LastAt):
				out[i].LastAt = c.LastAt
				if c.Title != "" {
					out[i].Title = c.Title
				}
			case out[i].Title == "":
				out[i].Title = c.Title
			}
			continue
		}
		byID[c.ID] = len(out)
		out = append(out, c)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].LastAt.After(out[j].LastAt) })
	if len(out) > maxChats {
		out = out[:maxChats]
	}
	return out
}

// rememberChat 记下一个会话。同一个群反复说话只更新标题与时间，不重复占位。
func (m *StreamManager) rememberChat(id, title string) {
	if id == "" {
		return
	}

	m.mu.Lock()
	now := time.Now()
	hit := -1
	for i := range m.chats {
		if m.chats[i].ID == id {
			hit = i
			break
		}
	}
	if hit >= 0 {
		if title != "" {
			m.chats[hit].Title = title
		}
		m.chats[hit].LastAt = now
	} else {
		m.chats = append(m.chats, ChatInfo{ID: id, Title: title, LastAt: now})
	}
	sort.SliceStable(m.chats, func(i, j int) bool {
		return m.chats[i].LastAt.After(m.chats[j].LastAt)
	})
	if len(m.chats) > maxChats {
		m.chats = m.chats[:maxChats]
	}

	// 落盘放到锁外：写 SQLite 也就几毫秒，但没必要在那期间卡住 Chats()。
	snapshot := append([]ChatInfo(nil), m.chats...)
	cs := m.chatStore
	m.mu.Unlock()

	if cs != nil {
		cs.SaveChats(snapshot)
	}
}

// run 维持长连接，断了就重连，直到 ctx 结束。
func (m *StreamManager) run(ctx context.Context, cfg Config) {
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
			// 连上过就从最短间隔重来：长期连接后偶发一次断线，
			// 不该接着用上次累积到 30 秒的等待，那会让恢复变得很慢。
			backoff = time.Second
		}
		if err != nil && !errors.Is(err, errReconnect) {
			m.setLastErr(err.Error())
			m.log.Warn("钉钉 Stream 连接中断，准备重连", "err", err, "retry_in", backoff.String())
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

// connectOnce 注册一次凭证、建一条连接、读到断开为止。
//
// 返回的 connected 表示「这次是否真的连上过」，调用方据此决定要不要把退避重置。
func (m *StreamManager) connectOnce(ctx context.Context, cfg Config) (connected bool, err error) {
	endpoint, ticket, err := m.openConnection(ctx, cfg)
	if err != nil {
		return false, err
	}

	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, wsURL(endpoint, ticket), nil)
	if err != nil {
		if resp != nil {
			return false, fmt.Errorf("WebSocket 握手失败（HTTP %d）: %w", resp.StatusCode, err)
		}
		return false, fmt.Errorf("WebSocket 握手失败: %w", err)
	}
	defer conn.Close()

	m.setLastErr("")
	connected = true
	m.log.Info("钉钉 Stream 长连接已建立", "endpoint", endpoint)

	// ctx 取消时主动关掉底层连接。
	// ReadMessage 不认 context，不关连接就会一直卡在那里 ——
	// 表现为「停止后连接还在、下一轮重连又多一条」，进程退出时也收不干净。
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	var wmu sync.Mutex
	write := func(v any) error {
		wmu.Lock()
		defer wmu.Unlock()
		// 30 秒写超时：对端半死时 WriteJSON 会一直等，把这个 goroutine 卡死，
		// 表现成「连接还在、回调再也不来」。
		_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		return conn.WriteJSON(v)
	}

	for {
		if ctx.Err() != nil {
			return connected, nil
		}
		_, raw, rerr := conn.ReadMessage()
		if rerr != nil {
			if ctx.Err() != nil {
				return connected, nil
			}
			return connected, fmt.Errorf("读取 Stream 消息失败: %w", rerr)
		}

		var f frame
		if uerr := json.Unmarshal(raw, &f); uerr != nil {
			m.log.Warn("钉钉 Stream 推送不是合法 JSON，已跳过", "err", uerr)
			continue
		}

		ack, needReconnect := m.handleFrame(f)
		if ack != nil {
			if werr := write(*ack); werr != nil {
				return connected, fmt.Errorf("回写 Stream 响应失败: %w", werr)
			}
		}
		if needReconnect {
			return connected, errReconnect
		}
	}
}

// handleFrame 分发一帧推送，返回要回写的响应与是否应当重连。
func (m *StreamManager) handleFrame(f frame) (*frameAck, bool) {
	messageID := f.Headers["messageId"]
	topic := f.Headers["topic"]

	switch f.Type {
	case "SYSTEM":
		switch topic {
		case systemTopicPing:
			// 探活要求「响应数据与推送数据完全一致」，opaque 原样带回去。
			// 这里做任何加工（比如重新序列化）都可能改变字段顺序或转义方式，
			// 钉钉按原样校验，改了就算失败。
			return &frameAck{Code: 200, Headers: ackHeaders(messageID), Message: "OK", Data: f.Data}, false
		case systemTopicDisconnect:
			// 不响应，直接重建连接；旧 ticket 也不能复用。
			m.log.Info("钉钉服务端要求断开长连接，正在重连")
			return nil, true
		}
		return &frameAck{Code: 200, Headers: ackHeaders(messageID), Message: "OK"}, false

	case "EVENT":
		// 没订阅业务事件，回个成功让服务端别重推。
		return &frameAck{Code: 200, Headers: ackHeaders(messageID), Message: "OK", Data: `{"status":"SUCCESS"}`}, false

	case "CALLBACK":
		// 卡片回执直接随 ACK 带回去：只有这样才能在点击的那一瞬间刷新卡片。
		// 早先这里固定回 {"response":{}}，等于点了之后什么都不发生 ——
		// 后端即使在跑，用户看到的也是「按了没反应」。
		reply := m.dispatchCallback(topic, f.Data)
		return &frameAck{Code: 200, Headers: ackHeaders(messageID), Message: "OK", Data: cardReplyData(reply)}, false
	}

	return &frameAck{Code: 200, Headers: ackHeaders(messageID), Message: "OK"}, false
}

// dispatchCallback 解析回调数据并交给业务侧，返回要当场写回卡片的变量。
//
// 解析失败只记日志、不抛错：ACK 已经要回了，让连接因为一条畸形消息断掉
// 会连带影响后面所有正常的回调。
func (m *StreamManager) dispatchCallback(topic, data string) map[string]string {
	switch topic {
	case TopicCardCallback:
		var cb CardCallback
		if err := json.Unmarshal([]byte(data), &cb); err != nil {
			m.log.Warn("解析钉钉卡片回调失败", "err", err)
			return nil
		}
		action := cb.Action()
		// 这行是排障锚点：只要它出现，就说明回调真的进来了，剩下的事全在业务侧。
		// content 原文一并打出来 —— 钉钉两批文档里 params 的层级不一样，
		// 与其猜结构，不如让用户点一次、把原文摊在日志里。
		m.log.Info("收到钉钉卡片回调",
			"out_track_id", cb.OutTrackID,
			"action", action,
			"content", clip(cb.rawContent(), 400))
		return m.handler.HandleDingTalkCard(cb.OutTrackID, action)

	case TopicBotMessage:
		var msg BotMessage
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			m.log.Warn("解析钉钉机器人消息失败", "err", err)
			return nil
		}
		m.rememberChat(msg.ConversationID, msg.ConversationTitle)
		m.handler.HandleDingTalkMessage(msg.ConversationID, msg.ConversationTitle)
		return nil

	default:
		m.log.Debug("未处理的钉钉回调 topic", "topic", topic)
		return nil
	}
}

// cardReplyData 把「要写回的卡片变量」拼成 Stream ACK 的 data。
//
// 结构是 {"response":{"cardData":{"cardParamMap":{…}}}}。官方两篇文档对
// 「按 key 局部覆盖」那个开关的字段名各执一词（cardUpdateOptions / cardOptions），
// 两个都带上：多余的那个会被忽略，少写一个就是整张卡片刷不动。
func cardReplyData(params map[string]string) string {
	const empty = `{"response":{}}`
	if len(params) == 0 {
		return empty
	}
	opts := map[string]any{"updateCardDataByKey": true}
	b, err := json.Marshal(map[string]any{
		"response": map[string]any{
			"cardData":          map[string]any{"cardParamMap": params},
			"cardUpdateOptions": opts,
			"cardOptions":       opts,
		},
	})
	if err != nil {
		return empty
	}
	return string(b)
}

func ackHeaders(messageID string) map[string]string {
	return map[string]string{
		"messageId":   messageID,
		"contentType": "application/json",
	}
}

// openConnection 注册 Stream 连接凭证。
func (m *StreamManager) openConnection(ctx context.Context, cfg Config) (endpoint, ticket string, err error) {
	req := map[string]any{
		"clientId":     cfg.ClientID,
		"clientSecret": cfg.ClientSecret,
		"subscriptions": []map[string]string{
			{"type": "CALLBACK", "topic": TopicBotMessage},
			{"type": "CALLBACK", "topic": TopicCardCallback},
		},
		// ua 只是给平台排查用，填一个能认出是哪个客户端的标识即可
		"ua": "AutoDocker-server/1.0.0",
	}

	var out struct {
		Endpoint string `json:"endpoint"`
		Ticket   string `json:"ticket"`
	}
	c := New(cfg)
	if err := c.doJSON(ctx, "POST", "/v1.0/gateway/connections/open", "", req, &out); err != nil {
		return "", "", fmt.Errorf("注册 Stream 连接失败: %w", err)
	}
	if out.Endpoint == "" || out.Ticket == "" {
		return "", "", fmt.Errorf("注册 Stream 连接失败: 返回缺少 endpoint 或 ticket")
	}
	return out.Endpoint, out.Ticket, nil
}

// wsURL 把 endpoint 与 ticket 拼成 WebSocket 地址。
// ticket 有效期只有 90 秒且只能用一次，不能缓存。
func wsURL(endpoint, ticket string) string {
	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}
	return endpoint + sep + "ticket=" + url.QueryEscape(ticket)
}

// frame 钉钉 Stream 的下行帧。
type frame struct {
	SpecVersion string            `json:"specVersion"`
	Type        string            `json:"type"`
	Headers     map[string]string `json:"headers"`
	Data        string            `json:"data"`
}

// frameAck 回写给钉钉的响应帧。
type frameAck struct {
	Code    int               `json:"code"`
	Headers map[string]string `json:"headers"`
	Message string            `json:"message"`
	Data    string            `json:"data,omitempty"`
}

// CardCallback 卡片按钮点击的回调数据。
//
// 按钮回传参数藏在 content 这层 JSON 字符串里 —— 钉钉给的是一个转义过的
// 字符串而不是对象，少解一层就永远取不到 action。
type CardCallback struct {
	OutTrackID string `json:"outTrackId"`
	Content    string `json:"content"`
	// Value 与 Content 内容完全一样。钉钉「事件回调」那批文档用 content，
	// 「消息卡片发送及更新」那批用 value，两批文档至今没统一，只好都读。
	Value   string `json:"value"`
	Type    string `json:"type"`
	SpaceID string `json:"spaceId"`
	UserID  string `json:"userId"`
}

// rawContent 回调里那份装着按钮信息的 JSON。
func (c CardCallback) rawContent() string {
	if c.Content != "" {
		return c.Content
	}
	return c.Value
}

// Action 取按钮回传参数里的 action。
//
// 钉钉的文档与实际推送对不上，一共三种长相，全得认：
//
//	{"cardPrivateData":{"params":{"action":"like"}}}                       新版文档示例
//	{"cardPrivateData":{...},"params":{"action":"agree"}}                  旧版（value 那批）
//	{"cardPrivateData":{"actionIds":["{\"action\":\"update\"}"],"params":{}}}  实际推送
//
// 第三种是实战里真正收到的：模板中「回传请求参数」填的原文被原样塞进
// actionIds 数组，params 则是空的。只按文档写就永远取到空 action ——
// 而空 action 会让真实卡片点下去什么都不做，看起来就是「按钮坏了」。
// 测试卡片反而正常，因为它压根不看 action，只看卡片 ID 前缀。
func (c CardCallback) Action() string {
	raw := c.rawContent()
	if raw == "" {
		return ""
	}
	var parsed struct {
		Params          map[string]any `json:"params"`
		CardPrivateData struct {
			Params    map[string]any `json:"params"`
			ActionIDs []string       `json:"actionIds"`
		} `json:"cardPrivateData"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return ""
	}
	for _, p := range []map[string]any{parsed.Params, parsed.CardPrivateData.Params} {
		if v, ok := p["action"].(string); ok && v != "" {
			return v
		}
	}
	for _, id := range parsed.CardPrivateData.ActionIDs {
		if act := actionFromParam(id); act != "" {
			return act
		}
	}
	return ""
}

// actionFromParam 把 actionIds 里的一项还原成 action 名。
//
// 模板里填什么这里就收到什么：填 {"action":"update"} 拿到的是这段 JSON 文本
// （注意它已经是字符串了，还得再解一层），填 update 就直接拿到 update。
// 两种都认，免得用户少打一层花括号就整个按钮失灵。
func actionFromParam(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "{") {
		return s
	}
	var v struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return ""
	}
	return v.Action
}

// BotMessage 机器人收到的消息（只取发现会话需要的字段）。
type BotMessage struct {
	ConversationID    string `json:"conversationId"`
	ConversationTitle string `json:"conversationTitle"`
	MsgType           string `json:"msgtype"`
	SenderNick        string `json:"senderNick"`
}

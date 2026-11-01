package dingtalk

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type fakeHandler struct {
	cards chan string
	chats chan string
	// reply 非空时作为卡片回执返回，用来验证 ACK 里真的带上了 cardData。
	reply map[string]string
}

func newFakeHandler() *fakeHandler {
	return &fakeHandler{cards: make(chan string, 8), chats: make(chan string, 8)}
}

func (f *fakeHandler) HandleDingTalkCard(outTrack, action string) map[string]string {
	f.cards <- outTrack + "|" + action
	return f.reply
}

func (f *fakeHandler) HandleDingTalkMessage(convID, title string) {
	f.chats <- convID + "|" + title
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mustJSON 把对象编成 JSON 字符串（回调的 data 字段本身是个字符串）。
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// echoFrame 服务端发送的一帧推送。
type echoFrame struct {
	SpecVersion string            `json:"specVersion"`
	Type        string            `json:"type"`
	Headers     map[string]string `json:"headers"`
	Data        string            `json:"data"`
}

// 端到端跑一遍协议：注册 → 建连 → 探活回显 → 卡片回调 → 机器人消息。
//
// 用一个假钉钉网关代替真服务：真卡片点击只能由组织里的真人触发，
// 但协议层是不变的，把帧收发钉死能覆盖掉绝大部分「点了没反应」的成因。
func TestStreamProtocolEndToEnd(t *testing.T) {
	h := newFakeHandler()
	// 让业务侧给出一个卡片回执，验证它真的搭上了 ACK 这班车。
	h.reply = map[string]string{"status": "正在更新"}
	var srv *httptest.Server
	var opens int32

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/gateway/connections/open", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&opens, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			// 用 ws:// 指向本测试服务，验证拼 ticket 的逻辑
			"endpoint": "ws" + strings.TrimPrefix(srv.URL, "http") + "/connect",
			"ticket":   "ticket-abc",
		})
	})
	mux.HandleFunc("/connect", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("ticket"); got != "ticket-abc" {
			t.Errorf("建连时 ticket = %q，期望 ticket-abc", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// 1) 探活：客户端必须把 data 原样回传
		_ = conn.WriteJSON(echoFrame{
			SpecVersion: "1.0", Type: "SYSTEM",
			Headers: map[string]string{"topic": "ping", "messageId": "m1", "contentType": "application/json"},
			Data:    `{"opaque":"opaque-xyz"}`,
		})
		var ack struct {
			Code    int               `json:"code"`
			Headers map[string]string `json:"headers"`
			Message string            `json:"message"`
			Data    string            `json:"data"`
		}
		if err := conn.ReadJSON(&ack); err != nil {
			t.Errorf("没收到探活响应: %v", err)
			return
		}
		if ack.Data != `{"opaque":"opaque-xyz"}` {
			t.Errorf("探活响应 data = %q，必须与请求完全一致（opaque 原样带回）", ack.Data)
		}
		if ack.Code != 200 || ack.Headers["messageId"] != "m1" {
			t.Errorf("探活响应 code/messageId 不对: %d / %q", ack.Code, ack.Headers["messageId"])
		}

		// 2) 卡片回调：action 藏在 content 这层 JSON 字符串里
		cardData := mustJSON(map[string]any{
			"outTrackId": "ad-7-1698560000000000000",
			"spaceType":  "im",
			"content": mustJSON(map[string]any{
				"cardPrivateData": map[string]any{
					"actionIds": []string{"btn_update"},
					"params":    map[string]any{"action": "update"},
				},
			}),
		})
		_ = conn.WriteJSON(echoFrame{
			SpecVersion: "1.0", Type: "CALLBACK",
			Headers: map[string]string{"topic": TopicCardCallback, "messageId": "m2"},
			Data:    cardData,
		})
		if err := conn.ReadJSON(&ack); err != nil {
			t.Errorf("卡片回调没有 ACK: %v", err)
			return
		}
		// 卡片回执必须随 ACK 一起回去。以前固定回 {"response":{}}，
		// 用户点下去卡片一动不动，看起来就是「按钮坏了」。
		if !strings.Contains(ack.Data, "cardParamMap") || !strings.Contains(ack.Data, "正在更新") {
			t.Errorf("卡片回调的 ACK 没带卡片数据，点了按钮卡片不会变: %s", ack.Data)
		}

		// 3) 机器人消息：用来发现会话
		_ = conn.WriteJSON(echoFrame{
			SpecVersion: "1.0", Type: "CALLBACK",
			Headers: map[string]string{"topic": TopicBotMessage, "messageId": "m3"},
			Data: mustJSON(map[string]any{
				"conversationId":    "cidXYZ=",
				"conversationTitle": "家里的群",
				"msgtype":           "text",
			}),
		})
		if err := conn.ReadJSON(&ack); err != nil {
			t.Errorf("消息回调没有 ACK: %v", err)
			return
		}

		// 4) 服务端要求断开：客户端应重连（旧 ticket 不可复用）
		_ = conn.WriteJSON(echoFrame{
			SpecVersion: "1.0", Type: "SYSTEM",
			Headers: map[string]string{"topic": "disconnect", "messageId": "m4"},
		})
		// 主动断开，让客户端走到重连分支
		time.Sleep(100 * time.Millisecond)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := NewStreamManager(ctx, h, quietLogger())
	m.Ensure(Config{ClientID: "id", ClientSecret: "sec", APIBase: srv.URL})

	select {
	case got := <-h.cards:
		if got != "ad-7-1698560000000000000|update" {
			t.Fatalf("卡片回调解析成 %q，期望 outTrackID|update", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("5 秒内没收到卡片回调")
	}

	select {
	case got := <-h.chats:
		if got != "cidXYZ=|家里的群" {
			t.Fatalf("会话回调解析成 %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("5 秒内没收到会话消息")
	}

	// 会话要记进列表，设置页「读取会话」才有东西可选
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(m.Chats()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if chats := m.Chats(); len(chats) != 1 || chats[0].ID != "cidXYZ=" {
		t.Fatalf("会话列表 = %+v，期望含 cidXYZ=", chats)
	}

	// disconnect 之后必须重新注册（新 ticket），否则连接就此死掉
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&opens) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := atomic.LoadInt32(&opens); n < 2 {
		t.Fatalf("服务端 disconnect 后没有重连（只注册了 %d 次）", n)
	}
}

// 注册失败时要把原因挂出来。设置页靠这条字符串告诉用户
// 「是凭据错了还是连不上」，只显示「未连接」等于什么都没说。
func TestStreamRegistrationErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"InvalidAuthentication","message":"无效的clientId"}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := NewStreamManager(ctx, newFakeHandler(), quietLogger())
	m.Ensure(Config{ClientID: "bad", ClientSecret: "bad", APIBase: srv.URL})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(m.LastError(), "InvalidAuthentication") {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("注册失败原因没有暴露出来，LastError = %q", m.LastError())
}

// 凭据不全时不建连，也不该有报错（用户还没填完，不是故障）。
func TestStreamSkipsWithoutCredentials(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := NewStreamManager(ctx, newFakeHandler(), quietLogger())
	m.Ensure(Config{ClientID: "", ClientSecret: ""})
	if m.Running() {
		t.Fatal("凭据为空时不该拉起长连接")
	}
	if m.LastError() != "" {
		t.Fatalf("凭据为空不算错误，却记了 %q", m.LastError())
	}
}

// 按钮回传参数在 content 的 JSON 字符串里，少解一层就永远拿不到 action。
func TestCardCallbackAction(t *testing.T) {
	cb := CardCallback{
		OutTrackID: "ad-3-1",
		Content:    `{"cardPrivateData":{"actionIds":["b1"],"params":{"action":"snooze"}}}`,
	}
	if got := cb.Action(); got != "snooze" {
		t.Fatalf("Action() = %q，期望 snooze", got)
	}

	if got := (CardCallback{Content: "不是 JSON"}).Action(); got != "" {
		t.Fatalf("非法 content 应返回空，得到 %q", got)
	}
	if got := (CardCallback{Content: `{"cardPrivateData":{"params":{}}}}`}).Action(); got != "" {
		t.Fatalf("没有 action 参数应返回空，得到 %q", got)
	}
}

// 钉钉两批文档给的层级不同，两种都得认出来。
//
// 只认一种的后果是「测试卡片一切正常、真实卡片点了没反应」：测试卡片
// 靠卡片 ID 前缀就能认出来，根本不读 action；真实卡片却必须靠 action
// 才知道该干什么。参数层级解不出来，action 就是空串，于是什么都不做。
func TestCardCallbackActionAcceptsBothShapes(t *testing.T) {
	cases := map[string]CardCallback{
		// 新版「事件回调」文档：params 嵌在 cardPrivateData 里
		"嵌套": {Content: `{"cardPrivateData":{"actionIds":["1"],"params":{"action":"update"}}}`},
		// 旧版「消息卡片发送及更新」文档：params 与 cardPrivateData 平级
		"平级": {Content: `{"cardPrivateData":{"actionIds":["1"]},"params":{"action":"update"}}`},
		// 同一份数据放在 value 里（两批文档对字段名也没统一）
		"value字段": {Value: `{"cardPrivateData":{"actionIds":["1"]},"params":{"action":"update"}}`},
	}
	for name, cb := range cases {
		if got := cb.Action(); got != "update" {
			t.Errorf("%s：Action() = %q，期望 update", name, got)
		}
	}

	// content 优先，两边都有时不该被 value 覆盖
	both := CardCallback{
		Content: `{"cardPrivateData":{"params":{"action":"ignore"}}}`,
		Value:   `{"cardPrivateData":{"params":{"action":"update"}}}`,
	}
	if got := both.Action(); got != "ignore" {
		t.Fatalf("content 与 value 都有时应以 content 为准，得到 %q", got)
	}
}

// 这条是实战抓包：钉钉真正推过来的长这样，params 是空的，
// 模板里「回传请求参数」的原文被塞进了 actionIds。
//
// 之前按文档写、只读 params，结果 action 恒为空 —— 日志里是
//
//	msg=收到钉钉卡片回调 action="" content="{\"cardPrivateData\":{\"actionIds\":[\"{\\\"action\\\":\\\"update\\\"}\"],\"params\":{}}}"
//
// 用户看到的就是「点了立即更新毫无反应」。所以这份原文必须进单测。
func TestCardCallbackActionReadsActionIDs(t *testing.T) {
	// 照抄日志里的 content，一个字符都不改
	const real = `{"cardPrivateData":{"actionIds":["{\"action\":\"update\"}"],"params":{}}}`

	if got := (CardCallback{Content: real}).Action(); got != "update" {
		t.Fatalf("实战结构里的 action 取不出来，得到 %q", got)
	}

	// 模板里少打一层花括号（直接填 update）也得能用，
	// 否则用户按文档「参数写 update」配完，按钮照样失灵。
	if got := (CardCallback{Content: `{"cardPrivateData":{"actionIds":["snooze"],"params":{}}}`}).Action(); got != "snooze" {
		t.Fatalf("actionIds 里是裸字符串时取不出来，得到 %q", got)
	}

	// 后面几个按钮不该被前一个动作带偏
	multi := `{"cardPrivateData":{"actionIds":["{\"action\":\"ignore\"}","{\"action\":\"update\"}"],"params":{}}}`
	if got := (CardCallback{Content: multi}).Action(); got != "ignore" {
		t.Fatalf("多个 actionId 时应取第一个，得到 %q", got)
	}

	// 畸形 JSON 不能 panic，也不能瞎猜出一个动作
	if got := (CardCallback{Content: `{"cardPrivateData":{"actionIds":["{坏掉的"],"params":{}}}`}).Action(); got != "" {
		t.Fatalf("畸形 actionId 应返回空，得到 %q", got)
	}
}

// ACK 里必须带上卡片数据，否则点了按钮卡片不会变样。
func TestCardReplyDataCarriesCardData(t *testing.T) {
	got := cardReplyData(map[string]string{"status": "正在更新"})
	if !strings.Contains(got, "cardParamMap") || !strings.Contains(got, "正在更新") {
		t.Fatalf("ACK 里必须带上卡片数据，否则点了按钮卡片不会变：%s", got)
	}
	if !strings.Contains(got, "updateCardDataByKey") {
		t.Fatalf("缺 updateCardDataByKey 会让模板里其他变量被清空：%s", got)
	}
	if !strings.Contains(got, `"response"`) {
		t.Fatalf("Stream 通道的回执要包一层 response：%s", got)
	}
	if got := cardReplyData(nil); got != `{"response":{}}` {
		t.Fatalf("没有回执时不该塞 cardData，实得 %s", got)
	}
}

// memChatStore 测试用的内存会话存储，代表「上一次运行留下的那份」。
type memChatStore struct {
	mu    sync.Mutex
	chats []ChatInfo
}

func (m *memChatStore) LoadChats() []ChatInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ChatInfo(nil), m.chats...)
}

func (m *memChatStore) SaveChats(c []ChatInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chats = append([]ChatInfo(nil), c...)
}

// 会话列表必须活过重启。
//
// 钉钉没有任何接口能反查「这个应用在哪些群里」，这份列表是收消息时攒出来的，
// 只放内存的话，装个包 / 改个配置重启一次就得让用户再 @ 一次机器人。
func TestChatsSurviveRestart(t *testing.T) {
	cs := &memChatStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := NewStreamManager(ctx, newFakeHandler(), quietLogger())
	first.AttachChatStore(cs)
	first.rememberChat("cid-group", "运维群")

	// 换一个实例 = 服务重启
	second := NewStreamManager(ctx, newFakeHandler(), quietLogger())
	second.AttachChatStore(cs)

	got := second.Chats()
	if len(got) != 1 || got[0].ID != "cid-group" || got[0].Title != "运维群" {
		t.Fatalf("重启后会话没读回来：%+v", got)
	}
}

// Attach 时内存里已有的（本进程刚收的）不能被磁盘上的旧数据盖掉。
func TestAttachChatStoreMergesInsteadOfReplacing(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	cs := &memChatStore{chats: []ChatInfo{
		{ID: "cid-a", Title: "旧标题", LastAt: old},
		{ID: "cid-b", Title: "另一个群", LastAt: old},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := NewStreamManager(ctx, newFakeHandler(), quietLogger())
	m.rememberChat("cid-a", "新标题") // 本进程先收到了一次
	m.AttachChatStore(cs)

	got := m.Chats()
	if len(got) != 2 {
		t.Fatalf("合并后应有 2 个会话，实得 %d 个：%+v", len(got), got)
	}
	if got[0].ID != "cid-a" || got[0].Title != "新标题" {
		t.Fatalf("同 ID 应保留更新的那份，实得 %+v", got[0])
	}
}

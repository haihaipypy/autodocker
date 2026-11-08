package wecom

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeHandler 记录业务侧收到的回调。
type fakeHandler struct {
	events   chan CardEvent
	sessions chan string
	reply    *TemplateCard
}

func newFakeHandler() *fakeHandler {
	return &fakeHandler{
		events:   make(chan CardEvent, 4),
		sessions: make(chan string, 4),
	}
}

func (f *fakeHandler) HandleWeComCard(ev CardEvent) *TemplateCard {
	f.events <- ev
	return f.reply
}

func (f *fakeHandler) HandleWeComSession(chatID, chatType, userID string) {
	f.sessions <- chatID + "|" + chatType + "|" + userID
}

// wsURL 把 httptest 的 http 地址换成 ws。
func wsURL(s *httptest.Server) string {
	return "ws" + strings.TrimPrefix(s.URL, "http")
}

// 整条链路：订阅 → 收卡片事件 → 用事件的 req_id 立刻回写卡片。
//
// 这一段是整个渠道的核心：企业微信只认「同 req_id 的回写」，
// req_id 弄错或用了自己生成的 ID，卡片就永远不会变，且服务端不报错。
func TestSubscribeThenRespondToCardEvent(t *testing.T) {
	replyCh := make(chan string, 1) // 收到的事件 req_id 与 task_id

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()

		// 1. 订阅
		var sub outFrame
		if err := c.ReadJSON(&sub); err != nil {
			return
		}
		if sub.Cmd != cmdSubscribe {
			t.Errorf("第一条命令应是 %s，得到 %s", cmdSubscribe, sub.Cmd)
			return
		}
		body, _ := sub.Body.(map[string]any)
		if body["bot_id"] != "bot-1" || body["secret"] != "sec-1" {
			t.Errorf("订阅应带上 bot_id / secret，得到 %+v", body)
		}
		reqID := sub.Headers["req_id"]
		_ = c.WriteJSON(map[string]any{
			"headers": map[string]string{"req_id": reqID},
			"errcode": 0,
			"errmsg":  "ok",
		})

		// 2. 推一个模板卡片按钮事件
		evReq := "evt-req-123"
		_ = c.WriteJSON(map[string]any{
			"cmd":     cmdEventCallback,
			"headers": map[string]string{"req_id": evReq},
			"body": map[string]any{
				"chatid":   "chat-g1",
				"chattype": "group",
				"from":     map[string]string{"userid": "u1"},
				"event": map[string]any{
					"eventtype": eventTemplateCard,
					"event_key": keyUpdate,
					"task_id":   "ad-9-1-1",
				},
			},
		})

		// 3. 期待一条回写
		var upd outFrame
		if err := c.ReadJSON(&upd); err != nil {
			return
		}
		if upd.Cmd != cmdRespondUpdate {
			t.Errorf("应回写 %s，实际 %s", cmdRespondUpdate, upd.Cmd)
			return
		}
		replyCh <- upd.Headers["req_id"]
		// 让连接多活一会儿，避免读循环在断言之前就报断开
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	h := newFakeHandler()
	h.reply = &TemplateCard{CardType: "button_interaction", TaskID: "ad-9-1-1"}
	m := New(t.Context(), h, quietLogger())
	m.endpoint = wsURL(srv)
	defer m.Stop()

	m.Ensure(Config{BotID: "bot-1", Secret: "sec-1", ChatID: "chat-g1"})

	select {
	case ev := <-h.events:
		if ev.TaskID != "ad-9-1-1" || ev.EventKey != keyUpdate || ev.ChatID != "chat-g1" {
			t.Fatalf("事件字段解析不对：%+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没收到卡片事件回调")
	}

	select {
	case reqID := <-replyCh:
		// 必须透传事件自带的 req_id，用自己生成的就等于没回
		if reqID != "evt-req-123" {
			t.Fatalf("回写必须用事件的 req_id，得到 %q", reqID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没有回写卡片")
	}
}

// 订阅被拒时要把 errcode 说清楚：用户填错 Secret 只看到「连不上」是没法排查的。
func TestSubscribeRejectedReportsCode(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		var sub outFrame
		if err := c.ReadJSON(&sub); err != nil {
			return
		}
		_ = c.WriteJSON(map[string]any{
			"headers": map[string]string{"req_id": sub.Headers["req_id"]},
			"errcode": 40001,
			"errmsg":  "invalid secret",
		})
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	m := New(t.Context(), newFakeHandler(), quietLogger())
	m.endpoint = wsURL(srv)
	defer m.Stop()
	m.Ensure(Config{BotID: "b", Secret: "bad"})

	// 重试是异步的，等它把错误记下来
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if msg := m.LastError(); strings.Contains(msg, "40001") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("LastError 应包含 errcode，得到 %q", m.LastError())
}

// 用户发来的消息只用来发现会话：单聊回调不带 chatid，必须退到 from.userid，
// 否则单聊用户永远填不上「接收会话」。
func TestMessageCallbackLearnsSession(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		var sub outFrame
		if err := c.ReadJSON(&sub); err != nil {
			return
		}
		_ = c.WriteJSON(map[string]any{
			"headers": map[string]string{"req_id": sub.Headers["req_id"]},
			"errcode": 0, "errmsg": "ok",
		})
		// 单聊：没有 chatid
		_ = c.WriteJSON(map[string]any{
			"cmd":     cmdMsgCallback,
			"headers": map[string]string{"req_id": "m1"},
			"body": map[string]any{
				"msgtype":  "text",
				"chattype": "single",
				"from":     map[string]string{"userid": "zhangsan"},
				"text":     map[string]string{"content": "hi"},
			},
		})
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	h := newFakeHandler()
	m := New(t.Context(), h, quietLogger())
	m.endpoint = wsURL(srv)
	defer m.Stop()
	m.Ensure(Config{BotID: "b", Secret: "s"})

	select {
	case got := <-h.sessions:
		if got != "zhangsan|single|zhangsan" {
			t.Fatalf("单聊应退到 userid 作会话 ID，得到 %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没收到会话回调")
	}

	chats := m.Chats()
	if len(chats) != 1 || chats[0].ID != "zhangsan" {
		t.Fatalf("会话列表应记下 zhangsan，得到 %+v", chats)
	}
}

// 回写卡片时，读循环必须还能继续读 —— 回执就走在同一条连接上。
//
// 1.5.4 的写法是在读循环里同步等回执，于是回执永远读不进来，
// 每次点击都卡满 5 秒报超时，服务端到底收没收到也不知道。
func TestRespondUpdateWaitsAckWithoutBlockingReadLoop(t *testing.T) {
	var logs syncBuf

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()

		var sub outFrame
		if err := c.ReadJSON(&sub); err != nil {
			return
		}
		_ = c.WriteJSON(map[string]any{
			"headers": map[string]string{"req_id": sub.Headers["req_id"]},
			"errcode": 0, "errmsg": "ok",
		})

		_ = c.WriteJSON(map[string]any{
			"cmd":     cmdEventCallback,
			"headers": map[string]string{"req_id": "evt-1"},
			"body": map[string]any{
				"chatid":   "chat-g1",
				"chattype": "group",
				"from":     map[string]string{"userid": "u1"},
				"event": map[string]any{
					"eventtype": eventTemplateCard,
					"event_key": "update:0",
					"task_id":   "ad-0-1-1",
				},
			},
		})

		var upd outFrame
		if err := c.ReadJSON(&upd); err != nil {
			return
		}
		if upd.Cmd != cmdRespondUpdate {
			t.Errorf("应回写 %s，实际 %s", cmdRespondUpdate, upd.Cmd)
			return
		}
		if upd.Cmd != cmdRespondUpdate {
			t.Errorf("应回写 %s，实际 %s", cmdRespondUpdate, upd.Cmd)
			return
		}
		// 线上真实形态必须过得了服务端校验：按钮交互型 + 至少一个按钮。
		// 换成 text_notice 就会因为缺 card_action 被拒（errcode=42045），
		// 这正是 1.5.5 手测里「卡片纹丝不动」的原因。
		raw, _ := json.Marshal(upd.Body)
		var sent struct {
			ResponseType string `json:"response_type"`
			TemplateCard struct {
				CardType   string `json:"card_type"`
				ButtonList []Btn  `json:"button_list"`
			} `json:"template_card"`
		}
		if err := json.Unmarshal(raw, &sent); err != nil {
			t.Errorf("回写体不是预期的 JSON：%v（%s）", err, raw)
			return
		}
		if sent.ResponseType != "update_template_card" {
			t.Errorf("response_type 应为 update_template_card，得到 %q", sent.ResponseType)
		}
		if sent.TemplateCard.CardType != "button_interaction" {
			t.Errorf("卡片类型必须是 button_interaction，得到 %q（text_notice 缺 card_action 会被拒）",
				sent.TemplateCard.CardType)
		}
		if len(sent.TemplateCard.ButtonList) == 0 {
			t.Error("按钮交互型卡片的 button_list 不能为空")
		}
		// 服务端收到并接受：回执的 req_id 与事件一致
		_ = c.WriteJSON(map[string]any{
			"headers": map[string]string{"req_id": "evt-1"},
			"errcode": 0, "errmsg": "ok",
		})
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	h := newFakeHandler()
	h.reply = &TemplateCard{CardType: "button_interaction", TaskID: "ad-0-1-1"}
	m := New(t.Context(), h, slog.New(slog.NewTextHandler(&logs, nil)))
	m.endpoint = wsURL(srv)
	defer m.Stop()
	m.Ensure(Config{BotID: "b", Secret: "s", ChatID: "chat-g1"})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), "企业微信卡片已更新") {
			if strings.Contains(logs.String(), "超时") {
				t.Fatalf("回执已到达却报超时：%s", logs.String())
			}
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("没等到卡片更新成功，日志：%s", logs.String())
}

// syncBuf 让测试里并发写的日志与主 goroutine 的读不打架。
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// 收到的帧里只要带 errcode 就是回执，不能当推送处理。
func TestInFrameDistinguishesAck(t *testing.T) {
	var pushed inFrame
	if err := json.Unmarshal([]byte(`{"cmd":"aibot_msg_callback","headers":{"req_id":"r"},"body":{}}`), &pushed); err != nil {
		t.Fatal(err)
	}
	if pushed.ErrCode != nil {
		t.Fatal("推送帧不该被当成回执")
	}

	var acked inFrame
	if err := json.Unmarshal([]byte(`{"headers":{"req_id":"r"},"errcode":0,"errmsg":"ok"}`), &acked); err != nil {
		t.Fatal(err)
	}
	if acked.ErrCode == nil || *acked.ErrCode != 0 {
		t.Fatal("回执帧必须识别出来，errcode=0 也算回执")
	}
}

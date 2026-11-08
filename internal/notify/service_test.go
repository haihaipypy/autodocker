package notify

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/notify/dingtalk"
	"github.com/haihaipypy/autodocker/internal/notify/feishu"
	"github.com/haihaipypy/autodocker/internal/notify/telegram"
	"github.com/haihaipypy/autodocker/internal/store"
)

// quietService 造一个只带 logger 的 Service，刻意不装配 store / updater / 渠道客户端。
//
// 这是有意为之：测试消息上的回调不关联任何通知，必须在碰数据库**之前**就被受理。
// 一旦有人把 ActionTest 分支挪到 GetNotification 后面，下面这些测试立刻会因为
// nil store 而 panic —— 正好把这类回归卡死在提交前。
func quietService() *Service {
	return &Service{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		dg:  dingtalk.New(dingtalk.Config{}),
		// 与 New 保持一致：开关默认全开，用户关掉的渠道由 ApplyNotifyEnabled 覆盖。
		enabled: map[model.Channel]bool{
			model.ChannelFeishu:   true,
			model.ChannelTelegram: true,
			model.ChannelDingTalk: true,
		},
	}
}

func TestHandleCardActionAcceptsTestCallback(t *testing.T) {
	s := quietService()

	toast, card := s.HandleCardAction(context.Background(), feishu.CardValue{Action: feishu.ActionTest})
	if toast == "" {
		t.Fatal("测试回调应返回一句 toast 反馈，否则群里点了没有任何提示")
	}
	if card == nil {
		t.Fatal("测试回调应返回回执卡片，让群里看到这一下点击真的生效了")
	}

	at, action, ch, count := s.CallbackState()
	if count != 1 {
		t.Fatalf("回调计数应为 1，得到 %d", count)
	}
	if action != feishu.ActionTest {
		t.Fatalf("记录的 action 应为 %q，得到 %q", feishu.ActionTest, action)
	}
	if ch != model.ChannelFeishu {
		t.Fatalf("应记录为飞书渠道，得到 %q", ch)
	}
	if at.IsZero() || time.Since(at) > time.Minute {
		t.Fatalf("回调时间戳不合理: %v", at)
	}
}

// Telegram 的测试按钮走另一条回调路径，同样必须在查库之前受理。
func TestHandleCallbackAcceptsTelegramTest(t *testing.T) {
	s := quietService()

	toast, msg := s.HandleCallback(context.Background(), telegram.CallbackValue{Action: telegram.ActionTest}, "123", 45)
	if toast == "" {
		t.Fatal("Telegram 测试回调应返回 toast，否则客户端按钮会一直转圈")
	}
	if msg == nil {
		t.Fatal("Telegram 测试回调应返回回执消息，原地替换掉带按钮的那条")
	}

	at, action, ch, count := s.CallbackState()
	if count != 1 || action != telegram.ActionTest || ch != model.ChannelTelegram {
		t.Fatalf("回调状态不符：count=%d action=%q channel=%q", count, action, ch)
	}
	if at.IsZero() {
		t.Fatal("回调时间戳不应为零值")
	}
}

// 设置页靠「有没有回调时间」区分「从未验证过」与「已验证」，初始必须是零值。
func TestCallbackStateZeroBeforeAnyCallback(t *testing.T) {
	at, action, ch, count := quietService().CallbackState()
	if !at.IsZero() || action != "" || ch != "" || count != 0 {
		t.Fatalf("初始状态应为零值，得到 %v / %q / %q / %d", at, action, ch, count)
	}
}

// 三个渠道的动作字符串必须完全一致：按钮里存的、落地时判断的，是同一批字面量。
// 任何一处改名却没同步，都会表现成「点了按钮提示未知操作」。
func TestActionConstantsAligned(t *testing.T) {
	pairs := []struct{ name, a, b string }{
		{"update", ActionUpdate, feishu.ActionUpdate},
		{"ignore", ActionIgnore, feishu.ActionIgnore},
		{"snooze", ActionSnooze, feishu.ActionSnooze},
		{"test", ActionTest, feishu.ActionTest},
	}
	for _, p := range pairs {
		if p.a != p.b {
			t.Fatalf("%s 动作常量与飞书不一致: %q vs %q", p.name, p.a, p.b)
		}
	}
	tgPairs := []struct{ name, a, b string }{
		{"update", ActionUpdate, telegram.ActionUpdate},
		{"ignore", ActionIgnore, telegram.ActionIgnore},
		{"snooze", ActionSnooze, telegram.ActionSnooze},
		{"test", ActionTest, telegram.ActionTest},
	}
	for _, p := range tgPairs {
		if p.a != p.b {
			t.Fatalf("%s 动作常量与 Telegram 不一致: %q vs %q", p.name, p.a, p.b)
		}
	}
}

// 显式选了某个渠道、但它没配好时，不能偷偷换成另一个 —— 那会让用户
// 明明选了 Telegram 却收到飞书消息，比收不到更让人困惑。
func TestEffectiveChannelRespectsSelection(t *testing.T) {
	s := quietService()
	s.fs = feishu.New(feishu.Config{AppID: "cli_x", AppSecret: "secret"})
	s.tg = telegram.New(telegram.Config{})

	s.ApplyChannel(string(model.ChannelTelegram))
	if got := s.EffectiveChannel(); got != "" {
		t.Fatalf("选了 Telegram 但没填 token 时应为空，得到 %q", got)
	}

	s.ApplyChannel(string(model.ChannelFeishu))
	if got := s.EffectiveChannel(); got != model.ChannelFeishu {
		t.Fatalf("飞书已配好且被选中时应返回 feishu，得到 %q", got)
	}

	// 未指定渠道时沿用历史行为：飞书优先
	s.ApplyChannel("")
	if got := s.EffectiveChannel(); got != model.ChannelFeishu {
		t.Fatalf("未指定渠道时应回落到飞书，得到 %q", got)
	}
}

// 两个渠道都填全了就应该都算「就绪」，一条提醒两边都发。
// 用户把两个渠道都配好了，只发其中一个会让人以为另一个坏掉了。
func TestReadyChannelsIncludesBoth(t *testing.T) {
	s := quietService()
	s.fs = feishu.New(feishu.Config{AppID: "cli_x", AppSecret: "secret", ChatID: "oc_x"})
	s.tg = telegram.New(telegram.Config{Token: "123:abc", ChatID: "-100123"})

	got := s.readyChannels()
	if len(got) != 2 || got[0] != model.ChannelFeishu || got[1] != model.ChannelTelegram {
		t.Fatalf("两个渠道都配好时应都就绪（飞书在前），得到 %v", got)
	}
}

// 凭据填了但没填接收会话，消息发不到任何地方，不能算就绪。
func TestReadyChannelsSkipsEmptyChatID(t *testing.T) {
	s := quietService()
	s.fs = feishu.New(feishu.Config{AppID: "cli_x", AppSecret: "secret"}) // 没有 ChatID
	s.tg = telegram.New(telegram.Config{Token: "123:abc", ChatID: "-100123"})

	got := s.readyChannels()
	if len(got) != 1 || got[0] != model.ChannelTelegram {
		t.Fatalf("飞书缺 chat_id 时不应就绪，得到 %v", got)
	}
}

// Notify 要把投递坐标填进 Deliveries，而不是只回一个主投递。
// 用假的 Bot API 顶替真接口，验证「发出去」和「记下来」是同一次调用完成的。
func TestNotifyRecordsDelivery(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			hits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":456,"date":1700000000}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	s := quietService()
	s.tg = telegram.New(telegram.Config{Token: "123:abc", ChatID: "-100123", APIBase: srv.URL})
	s.fs = feishu.New(feishu.Config{}) // 未配置，不参与

	n := &model.Notification{ImageRef: "demo/app:1.0", LatestTag: "1.1", Kind: model.NotifNewTag}
	msgID, err := s.Notify(context.Background(), n)
	if err != nil {
		t.Fatalf("推送失败: %v", err)
	}
	if hits != 1 {
		t.Fatalf("应调用一次 sendMessage，实际 %d 次", hits)
	}
	if msgID != "456" {
		t.Fatalf("返回的消息 ID 应为 456，得到 %q", msgID)
	}
	if len(n.Deliveries) != 1 || n.Deliveries[0].Channel != model.ChannelTelegram {
		t.Fatalf("投递记录不对: %+v", n.Deliveries)
	}
	if n.Deliveries[0].ChatRef != "-100123" || n.Deliveries[0].MessageID != "456" {
		t.Fatalf("投递坐标不对: %+v", n.Deliveries[0])
	}
	if n.Channel != model.ChannelTelegram || n.ChatRef != "-100123" {
		t.Fatalf("主投递应同步为 Telegram，得到 %q / %q", n.Channel, n.ChatRef)
	}
}

// 一个渠道挂了不能连累另一个：两边都配了却只收到一条，也比一条都没收到强。
func TestNotifySurvivesOneChannelFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"description":"boom"}`))
	}))
	defer srv.Close()

	s := quietService()
	s.tg = telegram.New(telegram.Config{Token: "123:abc", ChatID: "-100123", APIBase: srv.URL})
	s.fs = feishu.New(feishu.Config{}) // 未配置

	n := &model.Notification{ImageRef: "demo/app:1.0", LatestTag: "1.1", Kind: model.NotifNewTag}
	if _, err := s.Notify(context.Background(), n); err == nil {
		t.Fatal("唯一渠道失败时应返回错误，否则扫描器会把失败当成功记进日志")
	}
	if len(n.Deliveries) != 0 {
		t.Fatalf("失败时不应产生投递记录，得到 %+v", n.Deliveries)
	}
}

// 老库的通知没有 Deliveries，更新完成后仍要能改得动那唯一一条消息。
func TestDeliveryTargetsFallsBackToPrimary(t *testing.T) {
	s := quietService()

	old := &model.Notification{Channel: model.ChannelFeishu, ChatRef: "oc_x", MessageID: "om_1"}
	got := s.deliveryTargets(old)
	if len(got) != 1 || got[0].MessageID != "om_1" || got[0].Channel != model.ChannelFeishu {
		t.Fatalf("老数据应退回主投递，得到 %+v", got)
	}

	none := &model.Notification{}
	if got := s.deliveryTargets(none); got != nil {
		t.Fatalf("既无投递列表也无主投递时应返回 nil，得到 %+v", got)
	}
}

// 关掉的渠道不再推新提醒，但配置一个字节都不动。
// 用户关开关的意图是「先别发」，不是「删掉我的凭据」。
func TestReadyChannelsRespectsSwitch(t *testing.T) {
	s := quietService()
	s.fs = feishu.New(feishu.Config{AppID: "cli_x", AppSecret: "secret", ChatID: "oc_x"})
	s.tg = telegram.New(telegram.Config{Token: "123:abc", ChatID: "-100123"})

	s.ApplyNotifyEnabled(true, false, false, false)
	got := s.readyChannels()
	if len(got) != 1 || got[0] != model.ChannelFeishu {
		t.Fatalf("Telegram 关掉后只该剩飞书，得到 %v", got)
	}
	// 关掉不等于清空：凭据还在，重新打开要能立刻恢复
	if !s.tg.Configured() {
		t.Fatal("关开关不该清掉凭据")
	}

	s.ApplyNotifyEnabled(false, false, false, false)
	if got := s.readyChannels(); len(got) != 0 {
		t.Fatalf("全关后不该有任何渠道就绪，得到 %v", got)
	}
}

// 卡片 ID 里编着通知 ID。
//
// 钉钉的按钮回传参数由卡片模板定义，没有地方能像飞书那样把 nid 塞进按钮 value，
// 所以只能靠卡片实例 ID 反查是哪条通知 —— 解析错就会更新到别的容器。
func TestOutTrackIDRoundTrip(t *testing.T) {
	if got := parseOutTrackID(outTrackID(42)); got != 42 {
		t.Fatalf("往返后得到 %d，期望 42", got)
	}
	bad := []string{"", "ad-", "ad-x-1", "xx-42-1", "ad-0-1", "ad-42"}
	for _, s := range bad {
		if got := parseOutTrackID(s); got != 0 {
			t.Errorf("%q 应解析为 0（不能误判成某条通知），得到 %d", s, got)
		}
	}
}

// 同一秒内连推两条同一通知，卡片 ID 也不能撞 ——
// 钉钉按 outTrackId 唯一标识一张卡，撞了第二条就投放不进去。
func TestOutTrackIDUnique(t *testing.T) {
	a, b := outTrackID(7), outTrackID(7)
	if a == b {
		t.Fatalf("两次生成的卡片 ID 相同：%s", a)
	}
}

// 会话列表落在 settings 表里，重启后还能读出来。
//
// 用真库而不是假实现：这里要钉死的恰恰是「序列化 + 写库 + 再读出来」这条链，
// fake 掉就等于没测。
func TestDingTalkChatStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("开库失败：%v", err)
	}
	want := []dingtalk.ChatInfo{
		{ID: "cid-b", Title: "运维群", LastAt: time.Now().Truncate(time.Second)},
		{ID: "cid-a", Title: "", LastAt: time.Now().Add(-time.Hour).Truncate(time.Second)},
	}
	dingtalkChatStore{st: st}.SaveChats(want)
	st.Close()

	// 重开一个 = 服务重启后重新连库
	st2, err := store.Open(path)
	if err != nil {
		t.Fatalf("重开库失败：%v", err)
	}
	defer st2.Close()

	got := dingtalkChatStore{st: st2}.LoadChats()
	if len(got) != len(want) {
		t.Fatalf("读回 %d 条，期望 %d 条：%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Title != want[i].Title ||
			!got[i].LastAt.Equal(want[i].LastAt) {
			t.Fatalf("第 %d 条对不上：得 %+v，期望 %+v", i, got[i], want[i])
		}
	}
}

// 存坏了 / 没有 st 都不能 panic —— 这份数据随时能靠再 @ 一次机器人重建。
func TestDingTalkChatStoreToleratesBadData(t *testing.T) {
	if got := (dingtalkChatStore{}).LoadChats(); got != nil {
		t.Fatalf("没有 store 时应返回 nil，实得 %+v", got)
	}
	(dingtalkChatStore{}).SaveChats([]dingtalk.ChatInfo{{ID: "x"}}) // 不该 panic

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("开库失败：%v", err)
	}
	defer st.Close()
	if err := st.SetSetting(settingDingTalkChats, "{不是 JSON"); err != nil {
		t.Fatalf("写坏数据失败：%v", err)
	}
	if got := (dingtalkChatStore{st: st}).LoadChats(); got != nil {
		t.Fatalf("坏数据应忽略，实得 %+v", got)
	}
}

// New 必须把会话存储接上，否则落盘了也等于没读回来。
//
// 这条测试是「服务启动 → 设置页能选到上次记下的群」这条用户可见链路的唯一保障：
// 中间任何一环（New 忘了 attach、Attach 忘了 Load）都只会在装包重启后表现为
// 「读取会话又是空的」，本地跑起来完全正常。
func TestNewLoadsPersistedDingTalkChats(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("开库失败：%v", err)
	}
	defer st.Close()

	raw := `[{"id":"cid-x","title":"运维群","last_at":"2026-10-01T10:00:00Z"}]`
	if err := st.SetSetting(settingDingTalkChats, raw); err != nil {
		t.Fatalf("写库失败：%v", err)
	}

	svc := New(context.Background(), nil, st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	got := svc.DingTalkChats()
	if len(got) != 1 || got[0].ID != "cid-x" || got[0].Title != "运维群" {
		t.Fatalf("启动后没把上次记下的会话读回来：%+v", got)
	}
}

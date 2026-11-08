package wecom

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/haihaipypy/autodocker/internal/model"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// 卡片文案是「字符数」限制，不是字节数。
// 按字节切会把中文切成半个字，卡片上直接变成方块。
// 按钮 key 要能原样拆回「动作 + 通知 ID」。
//
// 这是 task_id 为空时的唯一凭据：主动推送的卡片被点击时，企业微信不回传
// task_id，只把 event_key 原样带回。key 拆不出来，所有按钮就都成了废键。
func TestButtonKeyRoundTrip(t *testing.T) {
	cases := []struct {
		key    string
		action string
		nid    int64
	}{
		{buttonKey(keyUpdate, 12), keyUpdate, 12},
		{buttonKey(keyIgnore, 0), keyIgnore, 0}, // 测试卡片：0 是合法值，不是「解析失败」
		{keyUpdate, keyUpdate, -1},              // 1.5.3 及以前发出去的老卡片
		{"", "", -1},
		{"update:", "update:", -1},
		{"update:x", "update:x", -1},
	}
	for _, c := range cases {
		action, nid := parseButtonKey(c.key)
		if action != c.action || nid != c.nid {
			t.Errorf("parseButtonKey(%q) = (%q, %d)，期望 (%q, %d)",
				c.key, action, nid, c.action, c.nid)
		}
	}
}

// 测试卡片的三个按钮都必须带 nid=0，服务端才能与「解析失败」区分开 ——
// 分不开的话，一张来历不明的卡片会被回一句「链路正常」，把故障说成成功。
func TestBuildTestCardKeysCarryZero(t *testing.T) {
	card := BuildTestCard("ad-0-1-1")
	for _, b := range card.ButtonList {
		if _, nid := parseButtonKey(b.Key); nid != 0 {
			t.Errorf("按钮 %q 的 key 应带 nid=0，得到 %q", b.Text, b.Key)
		}
	}
}

func TestClipCountsRunes(t *testing.T) {
	s := "新增版本可用请立即查看"
	got := clip(s, 4)
	if []rune(got)[len([]rune(got))-1] != '…' {
		t.Fatalf("截断后应以省略号结尾，得到 %q", got)
	}
	if n := len([]rune(got)); n != 4 {
		t.Fatalf("截断后应正好 4 个字符，得到 %d（%q）", n, got)
	}
	// 英文按字符也算得对
	if got := clip("abcdef", 10); got != "abcdef" {
		t.Fatalf("没超长不该改动，得到 %q", got)
	}
}

func TestShortDigest(t *testing.T) {
	cases := map[string]string{
		"sha256:0123456789abcdef0123": "0123456789ab",
		"sha256:abc":                  "abc",
		"":                            "—",
		"sha256:0123456789abcdef0123456789abcdef": "0123456789ab",
	}
	// 空串走 orDash 之前先看 shortDigest 自己：它只负责截断
	for in, want := range cases {
		if in == "" {
			continue
		}
		if got := shortDigest(in); got != want {
			t.Errorf("shortDigest(%q) = %q，想要 %q", in, got, want)
		}
	}
}

// 「发现新版本」的卡片必须带三个按钮，且 task_id 与投递记录一致 ——
// 企业微信的按钮回调只带 task_id，对不上就不知道该更新哪个容器。
func TestBuildUpdateCardHasButtons(t *testing.T) {
	n := &model.Notification{
		ID:            7,
		Title:         "nginx 有新版本",
		ImageRef:      "nginx:latest",
		ContainerName: "web",
		Kind:          model.NotifNewVersion,
		LocalSummary:  "sha256:aaaaaaaaaaaaaaaa",
		RemoteSummary: "sha256:bbbbbbbbbbbbbbbb",
	}
	card := BuildUpdateCard(n, "ad-7-1-1")

	if card.CardType != "button_interaction" {
		t.Fatalf("卡片类型应为 button_interaction，得到 %q", card.CardType)
	}
	if card.TaskID != "ad-7-1-1" {
		t.Fatalf("task_id 应原样带上，得到 %q", card.TaskID)
	}
	keys := []string{}
	for _, b := range card.ButtonList {
		keys = append(keys, b.Key)
	}
	want := []string{
		buttonKey(keyUpdate, 7), buttonKey(keySnooze, 7), buttonKey(keyIgnore, 7),
	}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("按钮 key 应为 %v，得到 %v", want, keys)
	}
	if card.MainTitle == nil || card.MainTitle.Title == "" {
		t.Fatal("主标题不能为空，否则卡片顶上是一片空白")
	}
	// 每个值都得守长度：超了企业微信不报错，只是把字截掉
	for _, it := range card.ContentList {
		if n := len([]rune(it.Value)); n > maxItemValue {
			t.Errorf("字段 %s 的值超长（%d 字）：%q", it.Keyname, n, it.Value)
		}
	}
}

// 状态卡片一律去掉按钮：重建任务同一时刻只允许一个，留着按钮只会让人再点一次。
func TestBuildStateCardHasNoButtons(t *testing.T) {
	n := &model.Notification{ImageRef: "redis:7", ContainerName: "cache"}
	card := BuildStateCard(n, "ad-3-1-1", model.CardDone, "容器已在新镜像上重建完成。")

	if len(card.ButtonList) != 0 {
		t.Fatalf("状态卡片不该有按钮，得到 %d 个", len(card.ButtonList))
	}
	if card.MainTitle == nil || !strings.Contains(card.MainTitle.Title, "完成") {
		t.Fatalf("标题应说明已完成，得到 %+v", card.MainTitle)
	}
	if card.SubTitleText == "" {
		t.Fatal("副标题应带上 detail，否则用户看不到失败原因")
	}
}

func TestBuildTestDoneCard(t *testing.T) {
	card := BuildTestDoneCard("ad-0-1-1", "2026-11-03 10:00:00")
	if len(card.ButtonList) != 0 {
		t.Fatal("回执卡片不该再带按钮")
	}
	if card.TaskID != "ad-0-1-1" {
		t.Fatal("task_id 必须保留，否则这张卡之后再点就找不到归属")
	}
}

// 回写卡片只有一种合法形状：button_interaction + 至少一个按钮。
//
// 这两个条件是企业微信的硬要求，踩了就是 errcode=42045（card_action Missing
// or Invalid）—— text_notice / news_notice 必须带 card_action 跳转地址，
// 而本程序没有可跳的地方。1.5.5 把无按钮卡片降级成 text_notice 正好踩中，
// 结果每次回写都被拒，用户看到的是「点了没反应」。
func TestNormalizeKeepsButtonTypeWithFallbackButton(t *testing.T) {
	done := BuildTestDoneCard("ad-0-1-1", "2026-11-03 10:00:00").normalized()
	if done.CardType != "button_interaction" {
		t.Fatalf("回执卡片必须保持 button_interaction，得到 %q", done.CardType)
	}
	if len(done.ButtonList) != 1 || done.ButtonList[0].Key != keyAck {
		t.Fatalf("无按钮的卡片应补一个 %q 按钮，得到 %+v", keyAck, done.ButtonList)
	}
	if done.CardAction != nil {
		t.Fatal("button_interaction 不需要 card_action，带上反而可能被判非法")
	}

	state := BuildStateCard(&model.Notification{ImageRef: "redis:7"}, "ad-3-1-1", model.CardDone, "已重建").normalized()
	if state.CardType != "button_interaction" || len(state.ButtonList) != 1 {
		t.Fatalf("状态卡片同样要能通过校验，得到 %q / %d 个按钮",
			state.CardType, len(state.ButtonList))
	}

	withBtn := BuildTestCard("ad-0-1-1").normalized()
	if withBtn.CardType != "button_interaction" || len(withBtn.ButtonList) != 3 {
		t.Fatalf("带业务按钮的卡片不能被改动，得到 %q / %d 个",
			withBtn.CardType, len(withBtn.ButtonList))
	}
}

// 任何一张卡片都不能变成 text_notice：那个类型强制要求 card_action。
func TestNormalizeNeverProducesTextNotice(t *testing.T) {
	cards := []TemplateCard{
		BuildTestCard("ad-0-1-1"),
		BuildTestDoneCard("ad-0-1-1", "2026-11-03 10:00:00"),
		BuildStateCard(&model.Notification{ImageRef: "redis:7"}, "ad-3-1-1", model.CardFailed, "拉取超时"),
		{},
	}
	for i, c := range cards {
		if got := c.normalized().CardType; got != "button_interaction" {
			t.Errorf("第 %d 张卡片类型为 %q，必须是 button_interaction", i, got)
		}
	}
}

// 结果消息要能区分三种收尾状态 —— 失败与回滚都说成「已完成」会误事。
func TestBuildDoneMessageStates(t *testing.T) {
	n := &model.Notification{ImageRef: "nginx:1.25", ContainerName: "web"}

	done := BuildDoneMessage(n, model.CardDone, "已重建")
	if !strings.Contains(done, "更新完成") || !strings.Contains(done, "web") {
		t.Fatalf("完成消息缺少关键信息：%q", done)
	}
	failed := BuildDoneMessage(n, model.CardFailed, "拉取超时")
	if !strings.Contains(failed, "更新失败") || !strings.Contains(failed, "拉取超时") {
		t.Fatalf("失败消息应说明原因：%q", failed)
	}
	rollback := BuildDoneMessage(n, model.CardRollback, "健康检查未通过")
	if !strings.Contains(rollback, "回滚") {
		t.Fatalf("回滚消息应说明已回滚：%q", rollback)
	}
}

// mergeChats 按 ID 去重，同一会话取更晚的时间与新标题，最后倒序。
func TestMergeChatsDedupes(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	now := time.Now()

	got := mergeChats(
		[]ChatInfo{{ID: "g1", Title: "旧名", ChatType: "group", LastAt: old}},
		[]ChatInfo{
			{ID: "g1", Title: "新名", ChatType: "group", LastAt: now},
			{ID: "u1", Title: "单聊 u1", ChatType: "single", LastAt: old},
		},
	)

	if len(got) != 2 {
		t.Fatalf("应按 ID 去重成 2 条，得到 %d：%+v", len(got), got)
	}
	if got[0].ID != "g1" || got[0].Title != "新名" {
		t.Fatalf("最近的那条应排在最前且用新标题，得到 %+v", got[0])
	}
	if got[1].ID != "u1" {
		t.Fatalf("次新的应是 u1，得到 %+v", got[1])
	}
}

func TestMergeChatsCaps(t *testing.T) {
	in := make([]ChatInfo, 0, maxChats+5)
	for i := 0; i < maxChats+5; i++ {
		in = append(in, ChatInfo{ID: "c" + string(rune('a'+i)), LastAt: time.Now().Add(time.Duration(i) * time.Second)})
	}
	if got := mergeChats(nil, in); len(got) != maxChats {
		t.Fatalf("列表应截断到 %d 条，得到 %d", maxChats, len(got))
	}
}

func TestMissingForSend(t *testing.T) {
	m := New(t.Context(), nil, quietLogger())
	if got := m.MissingForSend(); len(got) != 3 {
		t.Fatalf("什么都没填时三项都缺，得到 %v", got)
	}

	m.Ensure(Config{BotID: "b", Secret: "s", ChatID: "c"})
	if got := m.MissingForSend(); len(got) != 0 {
		t.Fatalf("填齐后不该再缺，得到 %v", got)
	}
	if !m.Ready() {
		t.Fatal("填齐三项应视为可发送")
	}
	m.Stop()
}

package feishu

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/haihaipypy/autodocker/internal/model"
)

// 飞书卡片 2.0 的字段名/层级一旦写错，本地完全看不出来——
// 要等线上真发一条才发现，而且报错往往只是一句「卡片解析失败」。
// 所以把结构约束固化成测试。
//
// 依据：飞书开放平台《卡片 JSON 2.0 结构》与《按钮》两篇文档。
func sampleNotification() *model.Notification {
	return &model.Notification{
		ID:            42,
		ImageRef:      "joplin/server:2.13.5",
		ContainerID:   "abc123",
		ContainerName: "joplin",
		Kind:          model.NotifNewVersion,
		Title:         "joplin 的镜像有更新",
		Body:          "远端内容已变更。",
		LocalSummary:  "aaaa1111",
		RemoteSummary: "bbbb2222",
		CardState:     model.CardPending,
	}
}

// collectTags 递归收集卡片里出现过的所有 tag。
func collectTags(v any, out map[string]int) {
	switch t := v.(type) {
	case map[string]any:
		if tag, ok := t["tag"].(string); ok {
			out[tag]++
		}
		for _, child := range t {
			collectTags(child, out)
		}
	case []any:
		for _, child := range t {
			collectTags(child, out)
		}
	}
}

func TestUpdateCardUsesSchemaV2(t *testing.T) {
	card := BuildUpdateCard(sampleNotification())

	if card["schema"] != "2.0" {
		t.Fatalf("必须声明 schema 2.0（1.0 的旧版回传交互不支持长连接），得到 %v", card["schema"])
	}

	// config.update_multi 在 2.0 下只支持 true
	cfg, ok := card["config"].(map[string]any)
	if !ok {
		t.Fatal("缺少 config")
	}
	if cfg["update_multi"] != true {
		t.Fatalf("config.update_multi 应为 true，得到 %v", cfg["update_multi"])
	}

	header, ok := card["header"].(map[string]any)
	if !ok {
		t.Fatal("缺少 header")
	}
	if _, ok := header["template"].(string); !ok {
		t.Fatalf("header.template 必须是字符串（如 blue/turquoise），得到 %#v", header["template"])
	}
	title, ok := header["title"].(map[string]any)
	if !ok || title["tag"] != "plain_text" {
		t.Fatalf("header.title 结构不对: %#v", header["title"])
	}

	body, ok := card["body"].(map[string]any)
	if !ok {
		t.Fatal("缺少 body")
	}
	elements, ok := body["elements"].([]any)
	if !ok || len(elements) == 0 {
		t.Fatalf("body.elements 不能为空: %#v", body["elements"])
	}

	tags := map[string]int{}
	collectTags(card, tags)
	for _, want := range []string{"markdown", "hr", "column_set", "column", "button", "plain_text"} {
		if tags[want] == 0 {
			t.Errorf("卡片里没有出现预期的 %q 组件，实际用到: %v", want, tags)
		}
	}
	// 卡片 2.0 已废弃交互模块，误用会导致整张卡片渲染失败
	if tags["action"] > 0 {
		t.Error("卡片 2.0 不支持 tag=action 的交互模块")
	}
}

// 「当前锁定」措辞读不通（用户实机反馈：`registry:2` 的卡片写成「当前锁定 2」），
// 改成「当前版本」。正文与字段列表要一起改，别只动一处。
func TestUpdateCardNewTagWording(t *testing.T) {
	n := sampleNotification()
	n.Kind = model.NotifNewTag
	n.LocalTag = "2"
	n.LatestTag = "3.1.2"
	n.Aliases = []string{"3.1.1", "3.1"}

	buf, err := json.Marshal(BuildUpdateCard(n))
	if err != nil {
		t.Fatal(err)
	}
	s := string(buf)
	if !strings.Contains(s, "当前版本") {
		t.Errorf("新版卡片应写「当前版本」，实际: %s", s)
	}
	if strings.Contains(s, "当前锁定") {
		t.Errorf("新版卡片不该出现「当前锁定」: %s", s)
	}
}

// 按钮的点击数据必须落在 behaviors[].value 上，否则回调里拿不到 nid，整条链路断掉。
func TestButtonsCarryCallbackValue(t *testing.T) {
	card := BuildUpdateCard(sampleNotification())

	var buttons []map[string]any
	findButtons(card, &buttons)
	if len(buttons) != 3 {
		t.Fatalf("预期 3 个按钮（立即更新/稍后提醒/忽略），实际 %d 个", len(buttons))
	}

	actions := map[string]bool{}
	for _, b := range buttons {
		behaviors, ok := b["behaviors"].([]any)
		if !ok || len(behaviors) == 0 {
			t.Fatalf("按钮缺少 behaviors: %#v", b)
		}
		first, ok := behaviors[0].(map[string]any)
		if !ok {
			t.Fatalf("behaviors 项类型不对: %#v", behaviors[0])
		}
		if first["type"] != "callback" {
			t.Fatalf("回传交互的 type 必须是 callback，得到 %v", first["type"])
		}

		// 值必须能原样还原成 CardValue，这是服务端解析按钮数据的实际路径
		buf, err := json.Marshal(first["value"])
		if err != nil {
			t.Fatal(err)
		}
		var v CardValue
		if err := json.Unmarshal(buf, &v); err != nil {
			t.Fatalf("按钮 value 无法还原: %v", err)
		}
		if v.NID != 42 || v.CID != "abc123" {
			t.Fatalf("按钮 value 丢了关键字段: %#v", v)
		}
		actions[v.Action] = true
	}

	for _, want := range []string{ActionUpdate, ActionSnooze, ActionIgnore} {
		if !actions[want] {
			t.Errorf("缺少 %q 动作的按钮，实际有 %v", want, actions)
		}
	}
}

// 按钮文字有 100 字符上限，超了卡片直接发不出去。
func TestButtonLabelsWithinLimit(t *testing.T) {
	for _, label := range []string{"立即更新", "稍后提醒", "忽略本版本", "重试更新"} {
		if len([]rune(label)) > 100 {
			t.Errorf("按钮文字 %q 超长", label)
		}
	}
}

// 失败态要给一个重试入口，成功态不给——避免用户误点。
func TestStateCardRetryButton(t *testing.T) {
	n := sampleNotification()

	failed := BuildStateCard(n, model.CardFailed, "拉取失败")
	var fb []map[string]any
	findButtons(failed, &fb)
	if len(fb) != 1 {
		t.Fatalf("失败态应有 1 个重试按钮，实际 %d 个", len(fb))
	}

	done := BuildStateCard(n, model.CardDone, "更新完成")
	var db []map[string]any
	findButtons(done, &db)
	if len(db) != 0 {
		t.Fatalf("成功态不应再有按钮，实际 %d 个", len(db))
	}

	rollback := BuildStateCard(n, model.CardRollback, "已回滚")
	var rb []map[string]any
	findButtons(rollback, &rb)
	if len(rb) != 1 {
		t.Fatalf("回滚态应给 1 个重试按钮，实际 %d 个", len(rb))
	}
}

// 测试卡片必须带一个可点的按钮。
//
// 「卡片收得到」和「按钮点得动」在飞书是两条彼此独立的链路：前者走
// im/v1/messages 发消息，后者走长连接回调。只发一张纯文本卡片，最常见的那类
// 故障（卡片正常显示、按钮点了毫无反应）根本暴露不出来。
// 回传载荷还必须是 ActionTest —— 服务端据此走「不查通知」的分支，
// 否则点测试按钮会提示「通知已失效」，反倒像是配置写错了。
func TestTestCardCarriesCallbackButton(t *testing.T) {
	var btns []map[string]any
	findButtons(BuildTestCard("oc_xxx"), &btns)
	if len(btns) != 1 {
		t.Fatalf("测试卡片应带 1 个验证按钮，实际 %d 个", len(btns))
	}

	behaviors, _ := btns[0]["behaviors"].([]any)
	if len(behaviors) == 0 {
		t.Fatal("验证按钮缺少 behaviors，点击后服务端拿不到回传数据")
	}
	first, _ := behaviors[0].(map[string]any)
	val, ok := first["value"].(CardValue)
	if !ok {
		t.Fatalf("回传载荷类型不对: %#v", first["value"])
	}
	if val.Action != ActionTest {
		t.Fatalf("回传 action 应为 %q，得到 %q", ActionTest, val.Action)
	}
}

// 卡片整体必须能序列化成 JSON —— 这是发消息接口真正传的东西。
func TestCardsAreJSONSerializable(t *testing.T) {
	n := sampleNotification()
	for name, card := range map[string]map[string]any{
		"update":    BuildUpdateCard(n),
		"done":      BuildStateCard(n, model.CardDone, "ok"),
		"ignore":    BuildStateCard(n, model.CardIgnored, "已忽略"),
		"test":      BuildTestCard("oc_xxx"),
		"test_done": BuildTestDoneCard(time.Now()),
	} {
		if _, err := json.Marshal(card); err != nil {
			t.Errorf("%s 卡片序列化失败: %v", name, err)
		}
	}
}

func findButtons(v any, out *[]map[string]any) {
	switch t := v.(type) {
	case map[string]any:
		if t["tag"] == "button" {
			*out = append(*out, t)
		}
		for _, child := range t {
			findButtons(child, out)
		}
	case []any:
		for _, child := range t {
			findButtons(child, out)
		}
	}
}

// 远端提醒必须把节点名列出来，而且要带上颜色；本机提醒不能凭空多出这一行。
//
// 手机上几条提醒连着弹，只靠标题里那串小字分不清是哪台机器，
// 这条断言就是把「节点单独一行 + 染色」钉住。
func TestBuildUpdateCardShowsNodeName(t *testing.T) {
	// 不用 json.Marshal：它会把 <font> 转成 \u003c，断言就看不出颜色了。
	render := func(n *model.Notification) string {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(BuildUpdateCard(n)); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	n := sampleNotification()
	n.NodeName = "1panel"
	n.Title = "🖥️ 1panel · joplin 有新版本可用"

	card := render(n)
	if !strings.Contains(card, "**节点**：<font color='orange'>1panel</font>") {
		t.Errorf("节点那一行没染色或没渲染: %s", card)
	}
	if !strings.Contains(card, "🖥️ 1panel") {
		t.Errorf("标题里应带节点名与符号: %s", card)
	}

	n.NodeName = ""
	if strings.Contains(render(n), "节点") {
		t.Error("本机提醒不该出现「节点」那一行")
	}
}

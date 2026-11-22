package telegram

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/haihaipypy/autodocker/internal/model"
)

// 按钮动作标识。
//
// 取值与 feishu 包里的同名常量必须完全一致：同一个更新任务可能来自任何一个渠道，
// 落地逻辑（notify.Service 里那套决定）只认这几个字符串。
// telegram_actions_test.go 里有一条断言把这件事钉死，改了一边会立刻红。
const (
	ActionUpdate = "update"
	ActionIgnore = "ignore"
	ActionSnooze = "snooze"
	ActionTest   = "test"
)

// Button 一行内联按钮。
type Button struct {
	Text string
	Data string
}

// Message 一条待发送的消息：正文 + 可选的按钮键盘。
type Message struct {
	Text   string
	Rows   [][]Button
	Silent bool
}

// markup 把按钮转成 Telegram 的 inline_keyboard。
// 没有按钮时返回 nil，交给调用方决定是否摘掉旧键盘。
func (m Message) markup() map[string]any {
	if len(m.Rows) == 0 {
		return nil
	}
	rows := make([][]map[string]any, 0, len(m.Rows))
	for _, row := range m.Rows {
		cells := make([]map[string]any, 0, len(row))
		for _, b := range row {
			cells = append(cells, map[string]any{"text": b.Text, "callback_data": b.Data})
		}
		rows = append(rows, cells)
	}
	return map[string]any{"inline_keyboard": rows}
}

// CallbackValue 按钮回传的载荷。
//
// Telegram 对 callback_data 有 **64 字节**硬限制（超了直接报 BUTTON_DATA_INVALID），
// 所以这里只用「动作 + 通知 ID」两个字段，紧凑编码成 `update:12`。
// 飞书那边可以把镜像引用、容器名一起塞进 value，这里不行 —— 需要的信息
// 落地时按通知 ID 从库里取，反正库里本来就有。
type CallbackValue struct {
	Action string
	NID    int64
}

// Encode 编码成 callback_data。
func (v CallbackValue) Encode() string {
	return v.Action + ":" + strconv.FormatInt(v.NID, 10)
}

// DecodeCallback 解析 callback_data。格式不对时返回错误，由调用方回一句提示。
func DecodeCallback(data string) (CallbackValue, error) {
	action, nidStr, ok := strings.Cut(strings.TrimSpace(data), ":")
	if !ok || action == "" {
		return CallbackValue{}, fmt.Errorf("无法识别的按钮数据: %q", data)
	}
	var nid int64
	if nidStr != "" {
		v, err := strconv.ParseInt(nidStr, 10, 64)
		if err != nil {
			return CallbackValue{}, fmt.Errorf("按钮数据里的通知 ID 非法: %q", nidStr)
		}
		nid = v
	}
	return CallbackValue{Action: action, NID: nid}, nil
}

// esc 转义 HTML 特殊字符。parse_mode 用的是 HTML，
// 镜像名里出现 & 或 < 的机会不多，但真出现时会把整条消息判成格式错误、发不出去。
func esc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// 通知正文由 notify/scanner 用 Markdown 片段拼成（`tag`、**版本**），
// 而 Telegram 走的是 HTML parse_mode —— 原样发出去会看到一堆反引号和星号。
// 这里只翻译这两种最常见的内联标记，其余保持原文。
var (
	mdCode = regexp.MustCompile("`([^`]+)`")
	mdBold = regexp.MustCompile(`\*\*([^*]+)\*\*`)
)

// rich 先转义再翻译内联标记。顺序不能反：
// 先转义才不会让正文里的 < 变成标签，后替换才轮到自己生成的标签生效。
func rich(s string) string {
	out := esc(s)
	out = mdCode.ReplaceAllString(out, "<code>$1</code>")
	out = mdBold.ReplaceAllString(out, "<b>$1</b>")
	return out
}

func orDash(s string) string {
	if s == "" {
		return "未知"
	}
	return s
}

// BuildUpdateMessage 构造「发现更新」消息，带立即更新 / 稍后 / 忽略三个按钮。
func BuildUpdateMessage(n *model.Notification) Message {
	head := "🔔 <b>" + rich(n.Title) + "</b>"
	if n.Kind == model.NotifNewTag {
		head = "🆙 <b>" + rich(n.Title) + "</b>"
	}

	name := n.ContainerName
	if name == "" {
		name = "（无关联容器）"
	}

	lines := []string{
		head,
		"",
		rich(n.Body),
		"",
	}
	// 节点放在容器前面并单独一行：手机上几条提醒连着弹，第一眼要能看出
	// 这条说的是哪台机器 —— 本机提醒没有这一行。
	if n.NodeName != "" {
		lines = append(lines, fmt.Sprintf("🖥️ 节点：<b>%s</b>", esc(n.NodeName)))
	}
	lines = append(lines,
		fmt.Sprintf("📦 容器：<code>%s</code>", esc(name)),
		fmt.Sprintf("🏷 镜像：<code>%s</code>", esc(n.ImageRef)),
	)
	if n.Kind == model.NotifNewTag && n.LatestTag != "" {
		lines = append(lines,
			fmt.Sprintf("当前版本：<code>%s</code>", esc(orDash(n.LocalTag))),
			fmt.Sprintf("可用版本：<code>%s</code>", esc(n.LatestTag)),
		)
	} else {
		lines = append(lines,
			fmt.Sprintf("本地摘要：<code>%s</code>", esc(orDash(n.LocalSummary))),
			fmt.Sprintf("远端摘要：<code>%s</code>", esc(orDash(n.RemoteSummary))),
		)
	}
	if len(n.Aliases) > 0 {
		lines = append(lines, "同版本别名："+esc(strings.Join(n.Aliases, "、")))
	}

	base := CallbackValue{NID: n.ID}
	return Message{
		Text: strings.Join(lines, "\n"),
		Rows: [][]Button{
			{
				{Text: "🚀 立即更新", Data: withAction(base, ActionUpdate).Encode()},
				{Text: "⏰ 稍后提醒", Data: withAction(base, ActionSnooze).Encode()},
			},
			{
				{Text: "🙈 忽略本版本", Data: withAction(base, ActionIgnore).Encode()},
			},
		},
	}
}

// BuildStateMessage 构造状态消息：更新中 / 成功 / 失败 / 已忽略 / 已稍后。
func BuildStateMessage(n *model.Notification, state model.CardState, detail string) Message {
	icon, title := "ℹ️", "已处理"
	switch state {
	case model.CardUpdating:
		icon, title = "⏳", "正在更新容器"
	case model.CardDone:
		icon, title = "✅", "更新完成"
	case model.CardFailed:
		icon, title = "❌", "更新失败"
	case model.CardRollback:
		icon, title = "↩️", "更新失败，已回滚"
	case model.CardIgnored:
		icon, title = "🙈", "已忽略该版本"
	case model.CardSnoozed:
		icon, title = "⏰", "已设为稍后提醒"
	}

	name := n.ContainerName
	if name == "" {
		name = n.ImageRef
	}

	lines := []string{
		fmt.Sprintf("%s <b>%s</b>", icon, esc(title)),
		"",
		rich(detail),
		"",
	}
	if n.NodeName != "" {
		lines = append(lines, fmt.Sprintf("🖥️ 节点：<b>%s</b>", esc(n.NodeName)))
	}
	lines = append(lines,
		fmt.Sprintf("📦 容器：<code>%s</code>", esc(name)),
		fmt.Sprintf("🏷 镜像：<code>%s</code>", esc(n.ImageRef)),
	)

	m := Message{Text: strings.Join(lines, "\n")}
	// 失败与回滚留一个「重试更新」，省得用户回后台页面点
	if state == model.CardFailed || state == model.CardRollback {
		base := CallbackValue{NID: n.ID, Action: ActionUpdate}
		m.Rows = [][]Button{{{Text: "🔁 重试更新", Data: base.Encode()}}}
	}
	return m
}

// BuildTestMessage 连通性测试消息，**带一个可点的按钮**。
//
// 与飞书中同一个道理：发消息与按钮回调是两条链路，
// 「收到测试消息」证明不了「按钮点得动」。测试消息必须带按钮，
// 点一下、消息原地变成回执，才算把整条链路验完。
func BuildTestMessage(chatHint string) Message {
	return Message{
		Text: strings.Join([]string{
			"✅ <b>第一步已完成</b>：这个会话能正常收到消息。",
			"",
			"👉 <b>第二步</b>：点下面的按钮 —— 消息变成「回调已收到」才算按钮交互也通了。",
			"",
			fmt.Sprintf("💬 接收会话：<code>%s</code>", esc(orDash(chatHint))),
			"",
			"<i>按钮没反应时，多半是 Bot Token 填错，或者这个 bot 已经在别处设了 webhook（两者互斥）。</i>",
		}, "\n"),
		Rows: [][]Button{{{Text: "👆 点我验证按钮回调", Data: CallbackValue{Action: ActionTest}.Encode()}}},
	}
}

// BuildTestDoneMessage 测试按钮点击后的回执。
func BuildTestDoneMessage(at time.Time) Message {
	return Message{Text: strings.Join([]string{
		"🎉 <b>按钮回调链路正常</b>",
		"",
		"服务端已经收到这次点击。发现镜像更新时，推送的消息按钮同样可以这样点，直接触发容器更新。",
		"",
		fmt.Sprintf("🕒 收到时间：%s", at.Format("2006-01-02 15:04:05")),
	}, "\n")}
}

func withAction(v CallbackValue, action string) CallbackValue {
	v.Action = action
	return v
}

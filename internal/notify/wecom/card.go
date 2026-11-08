package wecom

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/haihaipypy/autodocker/internal/model"
)

// 模板卡片的按钮动作。
const (
	keyUpdate = "update"
	keySnooze = "snooze"
	keyIgnore = "ignore"
	// keyAck 只是「把这张卡点掉」。状态卡片不带业务按钮，但按钮交互型卡片
	// 必须至少有一个按钮（见 normalized），所以给它留一个不办事的。
	keyAck = "ack"
)

// ActionAck 供业务侧识别「知道了」按钮。
const ActionAck = keyAck

// buttonKey 按钮 key = 动作 + 通知 ID。
//
// 为什么把通知 ID 塞进 key：模板卡片的 task_id 是「反查是哪条通知」的正规途径，
// 但实测主动推送（aibot_send_msg）的卡片被点击时，回调里的 task_id 是空字符串，
// 而 event_key 每次都原样回来。少这一个兜底，所有按钮都会因为「认不出是哪条通知」
// 被丢弃 —— 卡片发得出去、点了却永远没反应。
//
// task_id 仍然照发：它在「更新卡片」时是官方要求对齐的字段，
// 而且回复型卡片（走 replyTemplateCard）确实会回传，优先用它。
func buttonKey(action string, nid int64) string {
	return fmt.Sprintf("%s:%d", action, nid)
}

// parseButtonKey 拆回动作与通知 ID。不是本程序生成的格式时 nid 返回 -1。
//
// 用 -1 而不是 0 表示「无法解析」：0 是「测试卡片」的合法值，两者必须分开，
// 否则一张来历不明的卡片会被当成测试卡片回一句「链路正常」，把故障说成成功。
func parseButtonKey(key string) (action string, nid int64) {
	i := strings.LastIndex(key, ":")
	if i <= 0 || i == len(key)-1 {
		// 老格式（1.5.3 及以前发出去的卡片）：整个 key 就是动作名，没有通知 ID。
		return key, -1
	}
	v, err := strconv.ParseInt(key[i+1:], 10, 64)
	if err != nil || v < 0 {
		return key, -1
	}
	return key[:i], v
}

// TemplateCard 企业微信「按钮交互」模板卡片。
//
// 与钉钉最大的不同：这里的结构由**代码**定义，不需要去后台搭模板。
// 企业微信只校验字段合法性与长度，字段名写错的表现是「卡片上少一块」，
// 而不是报错 —— 所以字段长度上限要自己守住（见 clipText）。
type TemplateCard struct {
	CardType      string  `json:"card_type"`
	TaskID        string  `json:"task_id,omitempty"`
	MainTitle     *Title  `json:"main_title,omitempty"`
	SubTitleText  string  `json:"sub_title_text,omitempty"`
	ContentList   []Item  `json:"horizontal_content_list,omitempty"`
	ButtonList    []Btn   `json:"button_list,omitempty"`
	CardAction    *Action `json:"card_action,omitempty"`
	ActionMenu    *Menu   `json:"action_menu,omitempty"`
	EmphasisTitle *Emph   `json:"emphasis_content,omitempty"`
}

// normalized 保证卡片能通过服务端校验。
//
// 两条硬约束，违反任何一条都会得到 errcode=42045 / 40058，客户端纹丝不动：
//  1. text_notice / news_notice 必须带 card_action（type 1 跳 URL 或 2 开小程序），
//     而本程序没有任何可跳的地址 —— 所以卡片类型只能是 button_interaction。
//  2. button_interaction 必须至少有一个按钮。
//
// 1.5.5 之前把「无按钮」降级成 text_notice，正好两条都踩：类型本身要 card_action，
// 结果每次回写卡片都被拒，用户看到的就是「点了没反应」。
func (c TemplateCard) normalized() TemplateCard {
	if c.CardType == "" {
		c.CardType = "button_interaction"
	}
	if c.CardType == "button_interaction" && len(c.ButtonList) == 0 {
		c.ButtonList = []Btn{{Text: "知道了", Style: 2, Key: keyAck}}
	}
	return c
}

// Title 卡片主标题。
type Title struct {
	Title string `json:"title,omitempty"`
	Desc  string `json:"desc,omitempty"`
}

// Emph 关键数据样式。
type Emph struct {
	Title string `json:"title,omitempty"`
	Desc  string `json:"desc,omitempty"`
}

// Item 二级标题 + 文本，卡片上一行一条。
type Item struct {
	Keyname string `json:"keyname"`
	Value   string `json:"value,omitempty"`
}

// Btn 卡片按钮。Key 会被原样带进 event_key。
type Btn struct {
	Text  string `json:"text"`
	Style int    `json:"style,omitempty"`
	Key   string `json:"key"`
}

// Action 整张卡片（或跳转列表）的点击行为。
type Action struct {
	Type int    `json:"type,omitempty"`
	URL  string `json:"url,omitempty"`
}

// Menu 右上角「更多」菜单。
type Menu struct {
	Desc       string     `json:"desc"`
	ActionList []MenuNode `json:"action_list"`
}

// MenuNode 菜单里的一项。
type MenuNode struct {
	Text string `json:"text"`
	Key  string `json:"key"`
}

// 企业微信对卡片文案的长度限制（超出会被截断，不会报错，所以自己截）。
const (
	maxTitleLen   = 26
	maxDescLen    = 30
	maxSubTitle   = 112 // sub_title_text
	maxItemValue  = 26  // horizontal_content_list[].value
	maxButtonText = 10  // button_list[].text
)

// BuildUpdateCard 构造「发现新版本」的卡片。
//
// taskID 有两个作用：点按钮时随事件一起回来（企业微信不提供别的方式反查
// 是哪条通知），以及更新卡片时要与回调里的 task_id 对上。
func BuildUpdateCard(n *model.Notification, taskID string) TemplateCard {
	name := n.ContainerName
	if name == "" {
		name = "（无关联容器）"
	}

	items := []Item{{Keyname: "容器", Value: clip(name, maxItemValue)}}
	if n.Kind == model.NotifNewTag && n.LatestTag != "" {
		items = append(items,
			Item{Keyname: "镜像", Value: clip(n.ImageRef, maxItemValue)},
			Item{Keyname: "当前版本", Value: orDash(n.LocalTag)},
			Item{Keyname: "可用版本", Value: n.LatestTag},
		)
	} else {
		items = append(items,
			Item{Keyname: "镜像", Value: clip(n.ImageRef, maxItemValue)},
			Item{Keyname: "远端摘要", Value: orDash(shortDigest(n.RemoteSummary))},
		)
	}

	return TemplateCard{
		CardType:     "button_interaction",
		TaskID:       taskID,
		MainTitle:    &Title{Title: clip(n.Title, maxTitleLen), Desc: "AutoDocker 检测到镜像有新版本"},
		SubTitleText: "点「立即更新」按原配置重建容器，失败会自动回滚；暂时不处理可以点「稍后」。",
		ContentList:  items,
		ButtonList: []Btn{
			{Text: "立即更新", Style: 1, Key: buttonKey(keyUpdate, n.ID)},
			{Text: "稍后", Style: 2, Key: buttonKey(keySnooze, n.ID)},
			{Text: "忽略", Style: 2, Key: buttonKey(keyIgnore, n.ID)},
		},
	}
}

// BuildStateCard 构造某个状态下的卡片。
//
// 业务按钮一律去掉：企业微信的卡片一旦更新就换了整张，留着按钮会让用户以为
// 还能再点一次 —— 而重建任务同一时刻只允许一个，再点只会得到「已有任务」。
// normalized 会补一个「知道了」，那是卡片类型的硬性要求，不代表还能操作。
func BuildStateCard(n *model.Notification, taskID string, state model.CardState, detail string) TemplateCard {
	title, desc, color := statusOf(state)

	return TemplateCard{
		CardType:     "button_interaction",
		TaskID:       taskID,
		MainTitle:    &Title{Title: clip(title, maxTitleLen), Desc: clip(desc, maxDescLen)},
		SubTitleText: clip(orDash(detail), maxSubTitle),
		ContentList: []Item{
			{Keyname: "容器", Value: clip(orFallback(n.ContainerName, n.ImageRef), maxItemValue)},
			{Keyname: "镜像", Value: clip(n.ImageRef, maxItemValue)},
			{Keyname: "状态", Value: clip(title, maxItemValue)},
		},
		EmphasisTitle: &Emph{Title: clip(title, 10), Desc: clip(color, 15)},
	}
}

// statusOf 状态 → 标题 / 说明 / 一句话小结。
func statusOf(state model.CardState) (title, desc, summary string) {
	switch state {
	case model.CardUpdating:
		return "正在更新", "容器重建中，完成后会另发一条消息", "已受理"
	case model.CardDone:
		return "更新完成", "容器已在新镜像上重建", "成功"
	case model.CardFailed:
		return "更新失败", "详见后续消息", "失败"
	case model.CardRollback:
		return "已回滚", "新容器未通过健康检查，原容器已恢复", "已回滚"
	case model.CardIgnored:
		return "已忽略", "同一个版本不会再次提醒", "已忽略"
	case model.CardSnoozed:
		return "已稍后", "本轮巡检不再提醒，下次有更新会重新推送", "已稍后"
	default:
		return "已处理", "状态已记录", "已处理"
	}
}

// BuildTestCard 构造「发送测试」卡片。
//
// 必须带按钮：能收到卡片不代表按钮点得动 —— 发卡片走 aibot_send_msg，
// 按钮点击走 aibot_event_callback，是两条独立链路。
func BuildTestCard(taskID string) TemplateCard {
	return TemplateCard{
		CardType:     "button_interaction",
		TaskID:       taskID,
		MainTitle:    &Title{Title: "测试卡片", Desc: "点一下任意按钮，卡片会当场变样"},
		SubTitleText: "能收到这张卡片，说明机器人能往这个会话发消息；再点一下按钮，才能确认按钮回调也通。",
		ContentList: []Item{
			{Keyname: "用途", Value: "验证通知链路"},
			{Keyname: "不影响", Value: "不会更新任何容器"},
		},
		ButtonList: []Btn{
			{Text: "立即更新", Style: 1, Key: buttonKey(keyUpdate, 0)},
			{Text: "稍后", Style: 2, Key: buttonKey(keySnooze, 0)},
			{Text: "忽略", Style: 2, Key: buttonKey(keyIgnore, 0)},
		},
	}
}

// BuildTestDoneCard 测试卡片被点过之后的回执。
//
// 不带业务按钮：再点一次「立即更新」只会得到同一张卡。normalized 会补一个
// 「知道了」，那是按钮交互型卡片的硬性要求。
func BuildTestDoneCard(taskID, at string) TemplateCard {
	return TemplateCard{
		CardType:     "button_interaction",
		TaskID:       taskID,
		MainTitle:    &Title{Title: "按钮链路正常", Desc: "收到回调"},
		SubTitleText: "回调已收到，说明长连接、按钮事件、卡片更新三条都通了。这张卡片不会触发任何容器操作。",
		ContentList: []Item{
			{Keyname: "回调时间", Value: at},
			{Keyname: "结论", Value: "配置无误"},
		},
	}
}

// BuildDoneMessage 更新收尾时补发的那条消息。
//
// 为什么是「补发一条」而不是「改原来那张卡」：企业微信只允许在按钮事件的
// 5 秒内用该事件的 req_id 更新卡片，重建容器要几十秒，早就超时了。
// 原卡片会停在「正在更新」—— 那条消息里已经写明「完成后会另发一条消息」，
// 用户不会以为卡住了。
func BuildDoneMessage(n *model.Notification, state model.CardState, detail string) string {
	icon := "✅"
	title := "更新完成"
	switch state {
	case model.CardFailed:
		icon, title = "❌", "更新失败"
	case model.CardRollback:
		icon, title = "↩️", "已回滚"
	}

	name := orFallback(n.ContainerName, n.ImageRef)
	var b strings.Builder
	fmt.Fprintf(&b, "%s **%s**\n", icon, title)
	fmt.Fprintf(&b, "> 容器：%s\n", name)
	fmt.Fprintf(&b, "> 镜像：%s\n\n", n.ImageRef)
	b.WriteString(orDash(detail))
	return b.String()
}

// shortDigest 摘要只留前 12 位，卡片那一行放不下完整的 sha256。
func shortDigest(s string) string {
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// clip 按「字符数」截断。
//
// 企业微信的限制是按字数算的，直接切字节会把中文切成半个字（变成乱码方块）。
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func orFallback(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

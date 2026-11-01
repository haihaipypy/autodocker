package dingtalk

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/haihaipypy/autodocker/internal/model"
)

// 卡片模板里的三个变量名。
//
// 这是代码与「卡片平台」之间的硬约定：模板里变量名写成别的（哪怕只是大小写不同），
// 卡片发出去也是一片空白，而钉钉不会报错 —— 它只是找不到这个变量。
var (
	VarTitle  = "title"
	VarStatus = "status"
	VarBody   = "body"
)

// CardParams 钉钉卡片的变量取值。
//
// 钉钉只接受字符串，数字、布尔都得自己转成字符串再传；单个值上限 1KB。
type CardParams map[string]string

// BuildUpdateParams 构造「发现更新」的卡片数据。
func BuildUpdateParams(n *model.Notification) CardParams {
	name := n.ContainerName
	if name == "" {
		name = "（无关联容器）"
	}

	lines := []string{
		fmt.Sprintf("**容器**：%s", name),
		fmt.Sprintf("**镜像**：%s", n.ImageRef),
	}
	if n.Kind == model.NotifNewTag && n.LatestTag != "" {
		lines = append(lines,
			fmt.Sprintf("**当前版本**：%s", orDash(n.LocalTag)),
			fmt.Sprintf("**可用版本**：%s", n.LatestTag),
		)
	} else {
		lines = append(lines,
			fmt.Sprintf("**本地摘要**：%s", orDash(n.LocalSummary)),
			fmt.Sprintf("**远端摘要**：%s", orDash(n.RemoteSummary)),
		)
	}
	lines = append(lines, "",
		"点「立即更新」按原配置重建容器，更新失败会自动回滚；",
		"暂时不处理可以点「稍后」，不想再被这一版提醒可以点「忽略」。")

	return CardParams{
		VarTitle:  n.Title,
		VarStatus: "待更新",
		VarBody:   clip(strings.Join(lines, "\n"), 900),
	}
}

// BuildStateParams 构造某个状态下的卡片数据。
//
// 钉钉更新卡片是「按 key 覆盖变量」，卡片结构不变 —— 按钮会留在卡片上。
// 这与飞书 / Telegram 换掉整条消息不同，用户点过一次之后再点一次是可能的，
// 所以状态的措辞要让人一眼看出「已经处理过了」。
func BuildStateParams(n *model.Notification, state model.CardState, detail string) CardParams {
	status, title := "已处理", "已处理"
	switch state {
	case model.CardUpdating:
		status, title = "正在更新", "正在更新容器"
	case model.CardDone:
		status, title = "更新完成", "更新完成"
	case model.CardFailed:
		status, title = "更新失败", "更新失败"
	case model.CardRollback:
		status, title = "已回滚", "更新失败，已回滚"
	case model.CardIgnored:
		status, title = "已忽略", "已忽略该版本"
	case model.CardSnoozed:
		status, title = "已稍后", "已设为稍后提醒"
	}

	name := n.ContainerName
	if name == "" {
		name = n.ImageRef
	}
	body := fmt.Sprintf("**容器**：%s\n\n%s", name, orDash(detail))
	return CardParams{
		VarTitle:  title,
		VarStatus: status,
		VarBody:   clip(body, 900),
	}
}

// BuildTestParams 构造测试卡片。
//
// 第一句必须讲清「发得出去」只证明了一半：卡片能收到和按钮点得动是两条
// 独立的链路，混为一谈的话，用户会以为配好了，等真收到更新提醒才发现点不动。
func BuildTestParams(conversationHint string) CardParams {
	body := "**第一步已完成**：本群能正常接收卡片消息。\n" +
		"**第二步**：请点击下面的**任意一个按钮** —— 卡片变成「回调已收到」才算按钮交互也通了。\n\n" +
		"**接收群**：" + orDash(conversationHint)
	return CardParams{
		VarTitle:  "AutoDocker 通知测试",
		VarStatus: "等待点击",
		VarBody:   clip(body, 900),
	}
}

// BuildTestDoneParams 测试按钮点击后的回执。
func BuildTestDoneParams(atText string) CardParams {
	return CardParams{
		VarTitle:  "回调已收到",
		VarStatus: "回调正常",
		VarBody: clip("**按钮回调链路正常**：服务端已收到本次点击。\n\n"+
			"发现镜像更新时，推送的卡片按钮同样可以这样点击、直接触发容器更新。\n\n"+
			"**收到时间**："+atText, 900),
	}
}

// clip 按字节上限截断，但不切坏多字节字符。
//
// 钉钉对 cardParamMap 的单个值限制 1KB，超了整个请求会被判非法参数，
// 报错只说「param.invalid」，看不出是哪个变量太长。
func clip(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	const ellipsis = "…"
	limit := maxBytes - len(ellipsis)
	cut := 0
	for cut < len(s) {
		_, size := utf8.DecodeRuneInString(s[cut:])
		if cut+size > limit {
			break
		}
		cut += size
	}
	return s[:cut] + ellipsis
}

// orDash 空值统一显示成短横：卡片上一片空白会让人以为变量没传进去。
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

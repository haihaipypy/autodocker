package feishu

import (
	"fmt"
	"strings"
	"time"

	"github.com/haihaipypy/autodocker/internal/model"
)

// 按钮动作标识。
const (
	ActionUpdate = "update"
	ActionIgnore = "ignore"
	ActionSnooze = "snooze"
	// ActionTest 是测试卡片上那个按钮的动作：它不关联任何通知，
	// 唯一目的就是让服务端确认「按钮回调这条链路真的通」。
	ActionTest = "test"
)

// CardValue 卡片按钮点击后回传给服务的载荷。
// 字段刻意保持短小，飞书对按钮 value 体积有上限。
type CardValue struct {
	Action string `json:"action"`
	NID    int64  `json:"nid"`
	Ref    string `json:"ref"`
	CID    string `json:"cid"`
	Name   string `json:"name"`
}

// 注意：这里统一使用**卡片 2.0**（schema=2.0）格式。
// 卡片 1.0 的「消息卡片回传交互（旧）」回调不支持长连接订阅方式，
// 必须把回调发到开发者服务器（需要公网地址）；卡片 2.0 走 card.action.trigger，
// 可以用长连接接收，从而做到免公网部署。

func mdElement(content string) map[string]any {
	return map[string]any{"tag": "markdown", "content": content}
}

func hrElement() map[string]any {
	return map[string]any{"tag": "hr"}
}

func noteElement(content string) map[string]any {
	return map[string]any{"tag": "markdown", "content": "<font color='grey'>" + content + "</font>"}
}

// buttonElement 构造卡片 2.0 按钮，通过 behaviors 回传交互数据。
//
// 回传数据的落点是 behaviors 里的这条 callback 项，
// 点击后服务端收到的路径是 event.action.value（与这里写入的 CardValue 一一对应）。
func buttonElement(label, kind string, v CardValue) map[string]any {
	return map[string]any{
		"tag":  "button",
		"text": map[string]any{"tag": "plain_text", "content": label},
		"type": kind,
		"behaviors": []any{
			map[string]any{"type": "callback", "value": v},
		},
	}
}

// buttonRow 把若干个按钮并排放在一行里。
//
// 卡片 2.0 取消了 1.0 的「交互模块（tag=action）」，按钮的默认行为是各自独占一行。
// 想要横排得自己包一层 column_set —— 官方示例用的就是 flex_mode: flow。
func buttonRow(buttons ...map[string]any) map[string]any {
	columns := make([]any, 0, len(buttons))
	for _, b := range buttons {
		columns = append(columns, map[string]any{
			"tag":            "column",
			"width":          "auto",
			"weight":         1,
			"vertical_align": "top",
			"elements":       []any{b},
		})
	}
	return map[string]any{
		"tag":       "column_set",
		"flex_mode": "flow",
		"columns":   columns,
	}
}

func card2(headerTemplate, title string, elements []any) map[string]any {
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"update_multi": true,
		},
		"header": map[string]any{
			"template": headerTemplate,
			"title":    map[string]any{"tag": "plain_text", "content": title},
		},
		"body": map[string]any{
			"elements": elements,
		},
	}
}

// BuildUpdateCard 构造「发现更新」交互卡片，带立即更新 / 稍后 / 忽略三个按钮。
func BuildUpdateCard(n *model.Notification) map[string]any {
	template := "blue"
	if n.Kind == model.NotifNewTag {
		template = "turquoise"
	}

	name := n.ContainerName
	if name == "" {
		name = "（无关联容器）"
	}

	var lines []string
	// 远端提醒先亮出节点名并染上颜色：卡片标题里的名字字号小，
	// 连收几条时容易看串行，正文里再给一行橙色的才够醒目。
	if n.NodeName != "" {
		lines = append(lines, "**节点**："+nodeColor(n.NodeName))
	}
	lines = append(lines,
		fmt.Sprintf("**容器**：%s", name),
		fmt.Sprintf("**镜像**：%s", n.ImageRef),
	)
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
	if len(n.Aliases) > 0 {
		lines = append(lines, fmt.Sprintf("**同版本别名**：%s", strings.Join(n.Aliases, "、")))
	}

	base := CardValue{NID: n.ID, Ref: n.ImageRef, CID: n.ContainerID, Name: n.ContainerName}

	elements := []any{
		mdElement(n.Body),
		hrElement(),
		mdElement(strings.Join(lines, "\n")),
		hrElement(),
		buttonRow(
			buttonElement("立即更新", "primary_filled", withAction(base, ActionUpdate)),
			buttonElement("稍后提醒", "default", withAction(base, ActionSnooze)),
			buttonElement("忽略本版本", "danger", withAction(base, ActionIgnore)),
		),
		noteElement("AutoDocker · 点击即完成容器重建，原有挂载、端口与网络配置会完整保留"),
	}
	return card2(template, n.Title, elements)
}

// BuildStateCard 构造状态卡片：更新中 / 成功 / 失败 / 已忽略 / 已稍后。
// 点击按钮后原地替换原卡片，形成一条可追溯的操作记录。
func BuildStateCard(n *model.Notification, state model.CardState, detail string) map[string]any {
	template := "grey"
	title := "已处理"
	switch state {
	case model.CardUpdating:
		template, title = "orange", "正在更新容器"
	case model.CardDone:
		template, title = "green", "更新完成"
	case model.CardFailed:
		template, title = "red", "更新失败"
	case model.CardRollback:
		template, title = "red", "更新失败，已回滚"
	case model.CardIgnored:
		template, title = "grey", "已忽略该版本"
	case model.CardSnoozed:
		template, title = "yellow", "已设为稍后提醒"
	}

	name := n.ContainerName
	if name == "" {
		name = n.ImageRef
	}

	info := fmt.Sprintf("**容器**：%s\n**镜像**：%s", name, n.ImageRef)
	if n.NodeName != "" {
		info = "**节点**：" + nodeColor(n.NodeName) + "\n" + info
	}

	elements := []any{
		mdElement(detail),
		hrElement(),
		mdElement(info),
	}

	if state == model.CardFailed || state == model.CardRollback {
		base := CardValue{NID: n.ID, Ref: n.ImageRef, CID: n.ContainerID, Name: n.ContainerName}
		elements = append(elements, hrElement())
		elements = append(elements, buttonElement("重试更新", "primary", withAction(base, ActionUpdate)))
	}

	return card2(template, title, elements)
}

// BuildTestCard 连通性测试卡片，**带一个可点的按钮**。
//
// 为什么不能只发一张纯文本卡片：飞书这里有两条彼此独立的链路 ——
//
//	· 发消息走的是 tenant_access_token 调 im/v1/messages；
//	· 按钮点击是**回调**，走的是长连接（card.action.trigger）。
//
// 「消息发得出去」证明不了后者，现实里最常见的一类故障恰恰是卡片收得到、
// 按钮点了毫无反应。所以测试卡片必须带按钮，点一下、卡片原地变成回执，
// 才算把整条链路验完。
func BuildTestCard(chatHint string) map[string]any {
	return card2("green", "AutoDocker 通知测试", []any{
		mdElement("**第一步已完成**：本群能正常接收卡片消息。\n" +
			"**第二步**：请点击下面的按钮 —— 卡片变成「回调已收到」才算按钮交互也通了。"),
		hrElement(),
		mdElement(fmt.Sprintf("**接收群**：%s", orDash(chatHint))),
		hrElement(),
		buttonRow(buttonElement("点我验证按钮回调", "primary_filled", CardValue{Action: ActionTest})),
		noteElement("按钮无反应时，多半是「回调配置」里没加 card.action.trigger，或者改完没发布新版本"),
	})
}

// BuildTestDoneCard 测试按钮点击后的回执卡片：原地替换掉测试卡片。
func BuildTestDoneCard(at time.Time) map[string]any {
	return card2("green", "回调已收到", []any{
		mdElement("**按钮回调链路正常**：服务端已收到本次点击。\n" +
			"发现镜像更新时，推送的卡片按钮同样可以这样点击、直接触发容器更新。"),
		hrElement(),
		mdElement(fmt.Sprintf("**收到时间**：%s", at.Format("2006-01-02 15:04:05"))),
	})
}

func withAction(v CardValue, action string) CardValue {
	v.Action = action
	return v
}

func orDash(s string) string {
	if s == "" {
		return "未知"
	}
	return s
}

// nodeColor 节点名染成橙色，和本机提醒区分开。
//
// 卡片 2.0 的 markdown 支持 <font color=…>，颜色名用官方词表里的 orange；
// 写 RGB 或别的写法在客户端会被直接吞掉（只剩文字，没有报错）。
func nodeColor(node string) string {
	return "<font color='orange'>" + node + "</font>"
}

// Package wecom 接入企业微信「智能机器人」。
//
// 走的是官方长连接（WebSocket）模式，不要求公网 IP、不用加解密：
//
//	连上 wss://openws.work.weixin.qq.com → aibot_subscribe 认证 → 保持 30 秒心跳
//
// 与另外三个渠道最大的区别：**企业微信没有可用的 HTTP 发送接口**。
// 主动推送（aibot_send_msg）、更新卡片（aibot_respond_update_msg）都只能从
// 这条连接上发，所以连接管理器和客户端在这里合并成了一个 Manager。
//
// 还有一条必须提前知道的限制：卡片只有在「按钮事件到达后的 5 秒内」才能被更新，
// 且只能用该事件的 req_id。重建容器要几十秒，所以更新结果只能补发一条新消息，
// 原卡片会停在「正在更新」—— 这与飞书 / Telegram / 钉钉能事后改卡片不同。
package wecom

import "strings"

// Config 企业微信智能机器人的配置。
//
// 三个值都要在管理后台拿：机器人配置页开启「API 模式」→ 选「长连接」，
// 页面上会给出 BotID 与「长连接专用 Secret」（注意不是接收 URL 模式的
// Token / EncodingAESKey，两者不通用）。
type Config struct {
	// BotID 机器人的唯一标识。
	BotID string
	// Secret 长连接专用密钥。
	Secret string
	// ChatID 接收消息的会话：群聊填群的 chatid，单聊填对方的 userid。
	//
	// 不必手抄：让机器人在目标群里被 @ 一次（或单聊里说句话），
	// 设置页的「读取会话」就能把 ID 列出来点选。
	ChatID string
}

// Normalized 去掉首尾空白。
//
// 从聊天窗口复制 ID 时经常带不可见字符，留着会让认证或投递莫名其妙地失败，
// 而报错里又看不出区别。
func (c Config) Normalized() Config {
	return Config{
		BotID:  strings.TrimSpace(c.BotID),
		Secret: strings.TrimSpace(c.Secret),
		ChatID: strings.TrimSpace(c.ChatID),
	}
}

// HasCredentials 凭据是否齐全 —— 齐了才能拉起长连接。
func (c Config) HasCredentials() bool {
	return c.BotID != "" && c.Secret != ""
}

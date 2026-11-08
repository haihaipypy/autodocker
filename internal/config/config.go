package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 汇集全部运行时配置。所有项都可由环境变量覆盖，
// 环境变量仅作为首次启动的初值，之后以数据库中的设置为准。
type Config struct {
	// 基础
	Port      int
	DBPath    string
	StaticDir string
	LogLevel  string

	// Docker
	DockerHost string

	// 扫描
	ScanInterval     time.Duration
	Watch            []string
	RegistryInsecure bool
	RegistryMirror   string

	// 注册表认证（可选，格式 host=user:pass,host2=token）
	RegistryAuth map[string]string

	// 飞书自建应用
	FeishuAppID             string
	FeishuAppSecret         string
	FeishuChatID            string
	FeishuVerificationToken string
	FeishuEncryptKey        string
	FeishuUseLongConn       bool

	// 通知渠道与 Telegram
	NotifyChannel    string // 设置页当前打开的渠道，只影响显示哪份表单
	TelegramBotToken string
	TelegramChatID   string
	TelegramAPIBase  string
	TelegramProxy    string

	// 钉钉（企业内部应用 + Stream 长连接 + 互动卡片）
	DingTalkClientID       string
	DingTalkClientSecret   string
	DingTalkCardTemplateID string
	DingTalkConversationID string

	// 企业微信（智能机器人 + 长连接 + 模板卡片）
	WeComBotID  string
	WeComSecret string
	WeComChatID string

	// 更新策略
	AutoUpdateEnabled bool // 检测到更新后是否无需确认直接更新
	RequireConfirm    bool // 是否需要用户点击卡片按钮确认
	UpdateTimeout     time.Duration
	PullTimeout       time.Duration
	HealthCheckWait   time.Duration
	RollbackOnFailure bool
	SelfContainer     string // 自身容器名，禁止自更新
	SelfContainerID   string // 自身容器 ID（容器内 hostname 即短 ID）
	KeepOldContainer  bool   // 更新成功后是否保留旧容器（默认 false 删除）

	// 认证
	AdminUser     string
	AdminPassword string

	// Web
	PublicBaseURL string // 构建卡片回调/回跳链接用
}

// Load 从环境变量装载配置。
func Load() (*Config, error) {
	c := &Config{
		Port:                    envInt("PORT", 17777),
		DBPath:                  envStr("DB_PATH", "/data/autodocker.db"),
		StaticDir:               envStr("STATIC_DIR", "./web"),
		LogLevel:                envStr("LOG_LEVEL", "info"),
		DockerHost:              envStr("DOCKER_HOST", "unix:///var/run/docker.sock"),
		ScanInterval:            time.Duration(envInt("SCAN_INTERVAL", 3600)) * time.Second,
		Watch:                   envList("WATCH"),
		RegistryInsecure:        envBool("REGISTRY_INSECURE", false),
		RegistryMirror:          envStr("REGISTRY_MIRROR", ""),
		RegistryAuth:            parseAuth(envStr("REGISTRY_AUTH", "")),
		FeishuAppID:             envStr("FEISHU_APP_ID", ""),
		FeishuAppSecret:         envStr("FEISHU_APP_SECRET", ""),
		FeishuChatID:            envStr("FEISHU_CHAT_ID", ""),
		FeishuVerificationToken: envStr("FEISHU_VERIFICATION_TOKEN", ""),
		FeishuEncryptKey:        envStr("FEISHU_ENCRYPT_KEY", ""),
		FeishuUseLongConn:       envBool("FEISHU_USE_LONG_CONN", true),
		NotifyChannel:           envStr("NOTIFY_CHANNEL", ""),
		TelegramBotToken:        envStr("TELEGRAM_BOT_TOKEN", ""),
		TelegramChatID:          envStr("TELEGRAM_CHAT_ID", ""),
		TelegramAPIBase:         envStr("TELEGRAM_API_BASE", ""),
		TelegramProxy:           envStr("TELEGRAM_PROXY", ""),
		DingTalkClientID:        envStr("DINGTALK_CLIENT_ID", ""),
		DingTalkClientSecret:    envStr("DINGTALK_CLIENT_SECRET", ""),
		DingTalkCardTemplateID:  envStr("DINGTALK_CARD_TEMPLATE_ID", ""),
		DingTalkConversationID:  envStr("DINGTALK_CONVERSATION_ID", ""),
		WeComBotID:              envStr("WECOM_BOT_ID", ""),
		WeComSecret:             envStr("WECOM_SECRET", ""),
		WeComChatID:             envStr("WECOM_CHAT_ID", ""),
		AutoUpdateEnabled:       envBool("AUTO_UPDATE", false),
		RequireConfirm:          envBool("REQUIRE_CONFIRM", true),
		UpdateTimeout:           time.Duration(envInt("UPDATE_TIMEOUT", 300)) * time.Second,
		PullTimeout:             time.Duration(envInt("PULL_TIMEOUT", 1800)) * time.Second,
		HealthCheckWait:         time.Duration(envInt("HEALTH_CHECK_WAIT", 20)) * time.Second,
		RollbackOnFailure:       envBool("ROLLBACK_ON_FAILURE", true),
		SelfContainer:           envStr("SELF_CONTAINER", "autodocker"),
		SelfContainerID:         selfContainerID(),
		KeepOldContainer:        envBool("KEEP_OLD_CONTAINER", false),
		AdminUser:               envStr("ADMIN_USER", ""),
		AdminPassword:           envStr("ADMIN_PASSWORD", ""),
		PublicBaseURL:           strings.TrimRight(envStr("PUBLIC_BASE_URL", ""), "/"),
	}

	if c.Port <= 0 || c.Port > 65535 {
		return nil, fmt.Errorf("PORT 非法: %d", c.Port)
	}
	if c.ScanInterval > 0 && c.ScanInterval < 30*time.Second {
		c.ScanInterval = 30 * time.Second
	}
	if c.RequireConfirm && c.AutoUpdateEnabled {
		// 需要确认与全自动互斥，确认优先
		c.AutoUpdateEnabled = false
	}
	return c, nil
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// selfContainerID 推断本服务自身所在的容器 ID。
// 容器内 hostname 默认就是容器短 ID，可直接用于自我保护比对。
func selfContainerID() string {
	if v := strings.TrimSpace(os.Getenv("SELF_CONTAINER_ID")); v != "" {
		return v
	}
	if hn, err := os.Hostname(); err == nil {
		return strings.TrimSpace(hn)
	}
	return ""
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on", "y":
		return true
	case "0", "false", "no", "off", "n":
		return false
	}
	return def
}

func envList(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseAuth 解析 REGISTRY_AUTH="docker.io=user:pass,ghcr.io=token" 形式的注册表凭据。
func parseAuth(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := map[string]string{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		k, v, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

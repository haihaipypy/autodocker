package model

import "time"

// DetectMode 检测模式。
type DetectMode string

const (
	// ModeAuto 按 tag 形态自动分配：数字版本号走 pin-watch，其余走 digest-only。
	ModeAuto DetectMode = "auto"
	// ModeDigestOnly 只比对当前 tag 的远端 digest 是否变化（适合 latest 这类浮动 tag）。
	ModeDigestOnly DetectMode = "digest-only"
	// ModePinWatch 锁定 tag 的同时巡检仓库 tag 列表，发现更高版本时提醒。
	ModePinWatch DetectMode = "pin-watch"
)

// Status 镜像的当前判定状态。
type Status string

const (
	StatusUpToDate      Status = "up_to_date"
	StatusUpdatePending Status = "update_available" // 当前 tag 内容已变化
	StatusNewTag        Status = "new_version_tag"  // 仓库出现更高的版本 tag
	StatusUnknown       Status = "unknown"
	StatusIgnored       Status = "ignored"
)

// ImageOrigin 监控项的来源，决定它在页面上归到「容器」还是「镜像」。
//
// 这里刻意把「容器」和「镜像」分成两类记录，而不是混作一谈：
// 容器是「正在跑的东西」，更新它的代价是重建；镜像是「磁盘上的层」，
// 更新它只是重新拉取，删掉它也只是回收空间。两者的风险与操作完全不同，
// 混在一个列表里会让人分不清「我点更新到底动了什么」。
type ImageOrigin string

const (
	// OriginContainer 由本机容器发现：一条记录对应一个容器，
	// 更新动作 = 拉新镜像 + 按原配置重建该容器。
	OriginContainer ImageOrigin = "container"
	// OriginLocal 本机存在这个镜像，但没有任何容器在用它。
	// 页面上只给「删除」以回收磁盘 —— 有没有新版是容器维度的事，
	// 这里连状态都不展示，免得和容器的「可更新」混为一谈。
	OriginLocal ImageOrigin = "local"
	// OriginWatch 只来自 WATCH 配置的纯远端监控项，本机既无容器也无镜像。
	// 这种项只做「盯着仓库看有没有新版」，不涉及任何本地对象。
	OriginWatch ImageOrigin = "watch"
)

// Image 一个被监控的对象：可能是容器维度，也可能是镜像维度，见 ImageOrigin。
type Image struct {
	ID            int64       `json:"id"`
	Reference     string      `json:"reference"`      // 如 nginx:latest、ghcr.io/foo/bar:1.2.3
	ContainerID   string      `json:"container_id"`   // 关联的本地容器，非容器来源为空
	ContainerName string      `json:"container_name"` // 去掉前导 /
	Origin        ImageOrigin `json:"origin"`
	ImageID       string      `json:"image_id"`      // 本机镜像 ID（sha256:…），纯远端为空
	SizeBytes     int64       `json:"size_bytes"`    // 镜像占用字节，纯远端为 0
	ImageCreated  int64       `json:"image_created"` // 镜像构建时间（unix 秒）
	Dangling      bool        `json:"dangling"`      // 无 tag 的悬空镜像，无法比对版本
	LocalDigest   string      `json:"local_digest"`
	RemoteDigest  string      `json:"remote_digest"`
	LocalTag      string      `json:"local_tag"`
	LatestTag     string      `json:"latest_tag"` // 仓库中可用的更高版本 tag
	Aliases       []string    `json:"aliases"`    // 同 digest 的别名 tag
	Mode          DetectMode  `json:"mode"`
	ModeOverride  bool        `json:"mode_override"`
	Ignored       bool        `json:"ignored"`
	// IgnoredDigest 记录用户点「忽略本版本」时看到的那个远端摘要。
	//
	// 它与 Ignored 是两种粒度：Ignored 是「这个监控项我整个不要了」，
	// 一旦置位就不再查远端；IgnoredDigest 只针对**某一个版本**，
	// 远端摘要不变就不再打扰，仓库一旦推了新内容（摘要变了）照常通知。
	IgnoredDigest string    `json:"ignored_digest"`
	Status        Status    `json:"status"`
	BaselineAt    time.Time `json:"baseline_at"` // 首次纳入监控的时间
	LastCheckedAt time.Time `json:"last_checked_at"`
	LastError     string    `json:"last_error"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// LocalImage 是「镜像」标签页上的一行：本机真实存在的镜像，
// 叠加了库里对应的检测状态。它不落库，每次都是从 Docker 现读再拼装，
// 所以大小、创建时间这类信息永远是最新的。
type LocalImage struct {
	ID        string   `json:"id"`         // 镜像 ID，删除时用它最稳妥
	Name      string   `json:"name"`       // 镜像名（不含 tag）
	Tag       string   `json:"tag"`        // tag，无 tag 时为空
	Reference string   `json:"reference"`  // name:tag，无 tag 时为空
	SizeBytes int64    `json:"size_bytes"` // 占用字节（含共享层，仅作参考）
	Created   int64    `json:"created"`    // 构建时间（unix 秒）
	Dangling  bool     `json:"dangling"`   // 无 tag 的悬空镜像
	InUsed    bool     `json:"in_used"`    // 是否有容器在用它
	UsedBy    []string `json:"used_by"`    // 用到它的容器名
	// RemoteOnly 表示这只是「盯着远端仓库」的监控项，本机压根没有这份镜像：
	// 没有可删除的对象，也不该按「使用中 / 未使用」来归类。
	RemoteOnly bool   `json:"remote_only"`
	Status     Status `json:"status"`     // 检测状态，空串表示尚未纳入检测
	LatestTag  string `json:"latest_tag"` // 仓库中可用的更高版本 tag
	LastError  string `json:"last_error"`
	MonitorID  int64  `json:"monitor_id"` // 对应的库记录 ID，0 表示没有
	CheckMode  string `json:"check_mode"` // 检测模式的展示值
}

// NotificationKind 通知类型。
type NotificationKind string

const (
	// NotifNewVersion 当前 tag 指向的内容已变更（浮动 tag 场景）。
	NotifNewVersion NotificationKind = "new_version"
	// NotifNewTag 仓库出现了一个更高的独立版本 tag（锁定版本场景）。
	NotifNewTag NotificationKind = "new_tag"
)

// Channel 通知渠道。
//
// 同一时刻只有一个渠道是「生效渠道」：更新提醒只往那里发，按钮回调也只认它。
// 这样做的好处是每条通知归属明确 —— 后续要改状态时知道该去改哪条消息，
// 不必在多平台之间对账。想两个平台都收，等以后做「多渠道并存」再说。
type Channel string

const (
	ChannelFeishu   Channel = "feishu"
	ChannelTelegram Channel = "telegram"
	ChannelDingTalk Channel = "dingtalk"
	ChannelWeCom    Channel = "wecom"
)

// Valid 是否是已实现的渠道。
func (c Channel) Valid() bool {
	return c == ChannelFeishu || c == ChannelTelegram ||
		c == ChannelDingTalk || c == ChannelWeCom
}

// CardState 卡片/消息上承载的交互状态。
type CardState string

const (
	CardPending  CardState = "pending"  // 等待用户决定
	CardUpdating CardState = "updating" // 更新执行中
	CardDone     CardState = "done"     // 更新成功
	CardFailed   CardState = "failed"   // 更新失败
	CardRollback CardState = "rollback" // 更新失败已回滚
	CardIgnored  CardState = "ignored"  // 用户忽略
	CardSnoozed  CardState = "snoozed"  // 用户稍后
)

// Notification 一条更新提醒。
type Notification struct {
	ID            int64            `json:"id"`
	ImageRef      string           `json:"image_ref"`
	ContainerID   string           `json:"container_id"`
	ContainerName string           `json:"container_name"`
	Kind          NotificationKind `json:"kind"`
	Title         string           `json:"title"`
	Body          string           `json:"body"`
	LocalSummary  string           `json:"local_summary"`  // 本地镜像摘要短码
	RemoteSummary string           `json:"remote_summary"` // 远端镜像摘要短码
	LocalTag      string           `json:"local_tag"`      // 当前锁定的 tag
	LatestTag     string           `json:"latest_tag"`
	Aliases       []string         `json:"aliases"`
	Read          bool             `json:"read"`
	CardState     CardState        `json:"card_state"`

	// 这条通知推送到了哪个渠道、投递到哪个会话、对方的消息 ID。
	// 三者必须一起记：更新完成后要「原地改掉那条消息」，
	// 而不同渠道的定位方式不一样（飞书认 message_id，Telegram 要 chat_id + message_id）。
	//
	// 这两个字段是**主投递**（第一条发成功的），只为兼容老数据与前端展示而留。
	// 真正的全量投递记录在 Deliveries 里 —— 同时配了两个渠道时，一条提醒会各发一份。
	Channel   Channel `json:"channel"`
	ChatRef   string  `json:"chat_ref"`
	MessageID string  `json:"message_id"`

	Deliveries []Delivery `json:"deliveries,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// Delivery 一条通知在某一个渠道上的投递坐标。
//
// 为什么不能只留一个 channel/message_id：同时配了飞书和 Telegram 时，
// 同一条提醒会各发一份，两边的消息 ID 互不相干。
// 更新完成后要分别去改各自那条消息，否则另一个渠道会永远停在「待更新」。
type Delivery struct {
	Channel   Channel `json:"channel"`
	ChatRef   string  `json:"chat_ref"`
	MessageID string  `json:"message_id"`
}

// TaskStatus 更新任务状态。
type TaskStatus string

const (
	TaskPending    TaskStatus = "pending"
	TaskRunning    TaskStatus = "running"
	TaskSuccess    TaskStatus = "success"
	TaskFailed     TaskStatus = "failed"
	TaskRolledBack TaskStatus = "rolled_back"
)

// Task 一次容器更新任务。
type Task struct {
	ID             int64      `json:"id"`
	NotificationID int64      `json:"notification_id"`
	ImageRef       string     `json:"image_ref"`
	ContainerID    string     `json:"container_id"`
	ContainerName  string     `json:"container_name"`
	NewContainerID string     `json:"new_container_id"`
	Status         TaskStatus `json:"status"`
	TriggeredBy    string     `json:"triggered_by"` // feishu / web / auto
	Log            string     `json:"log"`
	Error          string     `json:"error"`
	BackupName     string     `json:"backup_name"` // 旧容器重命名后的名字，用于回滚
	StartedAt      *time.Time `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Scan 一次扫描记录。
type Scan struct {
	ID         int64     `json:"id"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Total      int       `json:"total"`
	WithUpdate int       `json:"with_update"`
	Notified   int       `json:"notified"`
	Forced     bool      `json:"forced"`
	Error      string    `json:"error"`
}

// Settings 可在线调整的运行时设置。
type Settings struct {
	ScanInterval      int    `json:"scan_interval"`       // 秒，0 表示关闭周期扫描
	AutoUpdate        bool   `json:"auto_update"`         // 无需确认直接更新
	RequireConfirm    bool   `json:"require_confirm"`     //
	RollbackOnFailure bool   `json:"rollback_on_failure"` //
	KeepOldContainer  bool   `json:"keep_old_container"`  //
	HealthCheckWait   int    `json:"health_check_wait"`   // 秒
	RegistryMirror    string `json:"registry_mirror"`
	RegistryInsecure  bool   `json:"registry_insecure"`

	// 设置页当前打开的那一栏（feishu / telegram / dingtalk），只影响显示哪份表单，
	// **不决定消息发给谁** —— 填好且开着开关的渠道都会收到。
	NotifyChannel string `json:"notify_channel"`

	// 每个渠道一个总开关：关掉后配置全部保留，只是不再往这里推新提醒。
	//
	// 用指针是为了区分「没设置过」与「显式关闭」：老库升级上来压根没有这几个字段，
	// 若把缺失一律当 false，用户在升级那一刻就会莫名收不到提醒。
	// 字段为 nil 时由 LoadSettings 按「凭据齐不齐」推导。
	FeishuEnabled   *bool `json:"feishu_enabled"`
	TelegramEnabled *bool `json:"telegram_enabled"`
	DingTalkEnabled *bool `json:"dingtalk_enabled"`
	WeComEnabled    *bool `json:"wecom_enabled"`

	// 飞书凭据放在设置里而不是只在环境变量里：应用装好后用户直接进页面填，
	// 不用回到安装向导；环境变量只作为首次启动的初值。
	FeishuAppID     string `json:"feishu_app_id"`
	FeishuAppSecret string `json:"feishu_app_secret"`
	FeishuChatID    string `json:"feishu_chat_id"`

	// Telegram：Bot Token 由 BotFather 生成，chat_id 决定消息落到哪个会话。
	// APIBase 可以换成自建反代 —— api.telegram.org 在部分网络下直连不通。
	// Proxy 是更省事的办法：家里 NAS 上通常已经跑着 Clash 之类，填它的 http 端口即可。
	TelegramBotToken string `json:"telegram_bot_token"`
	TelegramChatID   string `json:"telegram_chat_id"`
	TelegramAPIBase  string `json:"telegram_api_base"`
	TelegramProxy    string `json:"telegram_proxy"`

	// 钉钉：企业内部应用 + Stream 长连接（免公网）+ 互动卡片。
	// 卡片模板必须先在钉钉卡片平台搭好并发布，代码只填数据、不定义结构。
	// 会话 ID 由机器人在群里收到消息时自动汇报，不需要手工抄。
	DingTalkClientID       string `json:"dingtalk_client_id"`
	DingTalkClientSecret   string `json:"dingtalk_client_secret"`
	DingTalkCardTemplateID string `json:"dingtalk_card_template_id"`
	DingTalkConversationID string `json:"dingtalk_conversation_id"`

	// 企业微信：智能机器人 + 长连接（免公网）+ 模板卡片。
	// 与钉钉相反，卡片结构由代码定义，不用去后台搭模板；
	// 但卡片只能在按钮事件后 5 秒内更新，重建结果只能补发一条新消息。
	WeComBotID  string `json:"wecom_bot_id"`
	WeComSecret string `json:"wecom_secret"`
	WeComChatID string `json:"wecom_chat_id"`

	// 访问密码同样放在设置里：默认留空即不启用登录。
	// 本应用能重建任意容器（等同于系统最高权限），只在确实需要时开。
	AdminUser     string `json:"admin_user"`
	AdminPassword string `json:"admin_password"`

	// CheckUpdates 每天去 GitHub 看一眼自己有没有新版本，只提示、不自动安装。
	// 关掉就彻底不出网 —— 有些机器在内网，不想有任何多余的外连。
	CheckUpdates bool `json:"check_updates"`

	// SelfCheckProxy 自检访问 GitHub 时走的代理，形如 http://192.168.1.2:7890
	// 或 socks5h://192.168.1.2:1080。**通常不用填** —— 直连不通时程序会自动
	// 换公共加速；填了代理就不再走加速，因为代理更快更稳。
	SelfCheckProxy string `json:"selfcheck_proxy"`

	// SelfCheckMirror 手动指定 GitHub 加速前缀，形如 https://gh-proxy.com/。
	// 写法沿用 gh-proxy 系约定：把完整地址直接接在前缀后面。
	// 留空就用内置的公共加速清单；填了只走这一个，不再自动换。
	SelfCheckMirror string `json:"selfcheck_mirror"`
}

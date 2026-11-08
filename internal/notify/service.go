// Package notify 承担两件事：
//  1. 把扫描发现的更新推送成可交互消息（飞书卡片 / Telegram 带按钮的消息）；
//  2. 接收按钮点击，落更新任务并异步执行，再把结果写回同一条消息。
//
// 同时配了多个渠道时，一条提醒会**分别**发到每个就绪的渠道 ——
// 用户把两个渠道都填好了，就是两边都想收到，只发其中一个会让人以为坏掉了。
// 每条渠道的消息各记一份坐标（Notification.Deliveries），
// 更新完成后才改得动全部卡片，不会留下一个永远停在「待更新」的。
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/notify/dingtalk"
	"github.com/haihaipypy/autodocker/internal/notify/feishu"
	"github.com/haihaipypy/autodocker/internal/notify/telegram"
	"github.com/haihaipypy/autodocker/internal/notify/wecom"
	"github.com/haihaipypy/autodocker/internal/registry"
	"github.com/haihaipypy/autodocker/internal/store"
	"github.com/haihaipypy/autodocker/internal/updater"
	"github.com/haihaipypy/autodocker/internal/version"
)

// 与渠道无关的动作标识。
//
// 两个渠道包各自也定义了同名常量（按钮的 callback_data / value 里存的就是这几个字符串），
// service_test.go 里有一条断言把三处钉死，改了一边会立刻红。
const (
	ActionUpdate = "update"
	ActionIgnore = "ignore"
	ActionSnooze = "snooze"
	ActionTest   = "test"
)

// settingDingTalkChats 钉钉会话列表在 settings 表里的 key。
//
// 就存 settings 里：几条 {id,title,last_at}，量小到不值得单开一张表，
// 也就省掉了加表 / ALTER COLUMN 那套迁移 —— 那套在本项目里已经坑过一次。
const settingDingTalkChats = "dingtalk_chats"

// settingWeComChats 企业微信会话列表在 settings 表里的 key。
//
// 与钉钉同一个理由：企业微信没有「列出机器人所在群」的接口，
// 这份列表只能靠收消息攒出来，丢了就得让用户再 @ 一次机器人。
const settingWeComChats = "wecom_chats"

// TargetRef 计算本次更新真正要拉取的镜像引用。
//
// 两种通知的语义并不一样，这是最容易搞错的一处：
//   - 「浮动 tag 内容变更」：容器跑 nginx:latest，远端内容变了 → 还是拉 nginx:latest；
//   - 「仓库出现更高版本 tag」：容器跑 nginx:1.25，仓库里有 1.27 →
//     ImageRef 记录的仍是旧的 1.25，真正要拉的是 1.27。
//
// 因此不能在 new_tag 场景直接使用 ImageRef，否则会变成「把旧版本再拉一遍」的空操作。
func TargetRef(n *model.Notification) string {
	ref, err := registry.ParseReference(n.ImageRef)
	if err != nil {
		return n.ImageRef
	}
	// LatestTag 只在发现更高版本 tag 时才会被写入，且必然与当前 tag 不同
	if n.LatestTag == "" || n.LatestTag == ref.Tag {
		return n.ImageRef
	}
	return ref.WithTag(n.LatestTag)
}

// updateTarget 这一次更新实际要去的引用。
//
// 不能只信通知里记的 tag。通知可能是几天前「同一个 tag 内容变了」那版发出去的，
// 当时仓库里还没有更高的版本；等用户翻出来点，监控项里早记着 v3.7.0 了。
// 照旧通知的 v3.6.0 去重建，等于白拉一遍旧镜像 —— 耗时几十秒、卡片显示成功，
// 页面上的「可更新 v3.7.0」却一步没动。
//
// 有更高版本 tag 时一律以它为准，与网页上那个「更新」按钮的口径一致。
func (s *Service) updateTarget(n *model.Notification) string {
	target := TargetRef(n)
	img, err := s.monitorFor(n)
	if err != nil || img.LatestTag == "" {
		return target
	}
	ref, perr := registry.ParseReference(n.ImageRef)
	if perr != nil || version.Compare(img.LatestTag, ref.Tag) <= 0 {
		return target
	}
	return ref.WithTag(img.LatestTag)
}

// monitorFor 找到通知对应的监控项。
//
// 先按容器号找：升过 tag 之后这一行的 reference 还是旧的，
// 拿通知里的引用去精确匹配会扑空。
func (s *Service) monitorFor(n *model.Notification) (*model.Image, error) {
	if img, err := s.st.FindImageByContainer(n.ContainerID); err == nil {
		return img, nil
	}
	return s.st.FindImage(n.ImageRef, n.ContainerID)
}

// Service 通知与交互服务。
type Service struct {
	fs  *feishu.Client
	lc  *feishu.LongConnManager
	tg  *telegram.Client
	tp  *telegram.PollManager
	dg  *dingtalk.Client
	ds  *dingtalk.StreamManager
	wc  *wecom.Manager
	st  *store.Store
	up  *updater.Updater
	log *slog.Logger

	baseCtx context.Context

	// 设置里选的渠道（可能为空 = 未指定）。真正发消息时还要看这个渠道配没配。
	chMu    sync.RWMutex
	channel model.Channel

	// 每个渠道的总开关。关掉只停「推新提醒」，已发出的卡片照样回写状态 ——
	// 否则用户中途关掉渠道，之前那条消息会永远停在「待更新」。
	enMu    sync.RWMutex
	enabled map[model.Channel]bool

	// 按钮回调的观测记录。发消息与收按钮回调是两条彼此独立的链路：
	// 消息能收到并不代表按钮点得动。把最近一次回调记下来，
	// 设置页就能给出确定结论，用户不必去翻容器日志猜。
	cbMu     sync.Mutex
	cbAt     time.Time
	cbAction string
	cbChan   model.Channel
	cbCount  int
}

// New 构造通知服务。baseCtx 用于派生异步更新任务的上下文。
func New(ctx context.Context, fs *feishu.Client, st *store.Store, up *updater.Updater, logger *slog.Logger) *Service {
	svc := &Service{
		fs:      fs,
		tg:      telegram.New(telegram.Config{}),
		dg:      dingtalk.New(dingtalk.Config{}),
		st:      st,
		up:      up,
		log:     logger,
		baseCtx: ctx,
		// 构造时先全开：ApplySettings 要等设置读出来才下发，
		// 这中间若恰好跑完一轮扫描，按「默认关闭」处理会平白吞掉一条提醒。
		// 用户真正关掉的开关会在 ApplySettings 里立刻覆盖掉这三个默认值。
		enabled: map[model.Channel]bool{
			model.ChannelFeishu:   true,
			model.ChannelTelegram: true,
			model.ChannelDingTalk: true,
			model.ChannelWeCom:    true,
		},
	}
	// Service 自己就是按钮回调的处理方，直接把 this 交给各渠道的连接管理器
	svc.lc = feishu.NewLongConnManager(ctx, svc, logger)
	svc.tp = telegram.NewPollManager(ctx, svc, logger)
	svc.ds = dingtalk.NewStreamManager(ctx, svc, logger)
	// 会话列表必须落盘：钉钉没有任何接口能反查「这个应用在哪些群里」，
	// 这份列表是收消息时攒出来的，丢了就只能让用户再 @ 一次机器人。
	svc.ds.AttachChatStore(dingtalkChatStore{st: st, log: logger})

	// 企业微信同理：唯一能拿到 chatid 的时机就是机器人在群里收到消息。
	svc.wc = wecom.New(ctx, svc, logger)
	svc.wc.AttachChatStore(wecomChatStore{st: st, log: logger})
	return svc
}

// Feishu 暴露底层飞书客户端，供 API 层做配置与测试。
func (s *Service) Feishu() *feishu.Client { return s.fs }

// Telegram 暴露底层 Telegram 客户端。
func (s *Service) Telegram() *telegram.Client { return s.tg }

// DingTalk 暴露底层钉钉客户端。
func (s *Service) DingTalk() *dingtalk.Client { return s.dg }

// WeCom 暴露底层企业微信管理器，供 API 层做配置、测试与会话读取。
func (s *Service) WeCom() *wecom.Manager { return s.wc }

// ApplyNotifyEnabled 下发四个渠道的开关状态。
//
// 关掉只影响「要不要推新提醒」，不断开回调链路 ——
// 断了的话，用户重新打开之前发出去的那批卡片就再也改不动状态了。
func (s *Service) ApplyNotifyEnabled(feishu, telegramCh, dingtalkCh, wecomCh bool) {
	s.enMu.Lock()
	s.enabled[model.ChannelFeishu] = feishu
	s.enabled[model.ChannelTelegram] = telegramCh
	s.enabled[model.ChannelDingTalk] = dingtalkCh
	s.enabled[model.ChannelWeCom] = wecomCh
	s.enMu.Unlock()
}

// channelEnabled 渠道开关是否打开。没下发过一律按关闭处理：
// 宁可少发一条，也不要在一个用户明确没说「要」的渠道上发消息。
func (s *Service) channelEnabled(ch model.Channel) bool {
	s.enMu.RLock()
	defer s.enMu.RUnlock()
	return s.enabled[ch]
}

// ApplyChannel 记录用户选的渠道。留空表示「谁配好了就用谁」，飞书优先。
func (s *Service) ApplyChannel(ch string) {
	s.chMu.Lock()
	defer s.chMu.Unlock()
	s.channel = model.Channel(ch)
}

// EffectiveChannel 设置页当前选中的渠道，仅用于展示与默认值。
//
// **不再决定消息发给谁** —— 1.2.10 起一条提醒会广播到所有就绪渠道（见 readyChannels）。
// 显式选了某个渠道但它没配好时返回空，而不是偷偷换成另一个。
func (s *Service) EffectiveChannel() model.Channel {
	s.chMu.RLock()
	ch := s.channel
	s.chMu.RUnlock()

	switch ch {
	case model.ChannelFeishu:
		if s.fs.Configured() {
			return model.ChannelFeishu
		}
		return ""
	case model.ChannelTelegram:
		if s.tg.Configured() {
			return model.ChannelTelegram
		}
		return ""
	}
	// 未指定：保持历史行为，飞书优先
	if s.fs.Configured() {
		return model.ChannelFeishu
	}
	if s.tg.Configured() {
		return model.ChannelTelegram
	}
	return ""
}

// NotifyReady 是否至少有一个渠道能把消息真正发出去。
//
// 改成多渠道广播之后，一条提醒会发到**所有**就绪渠道，
// 所以这里问的是「有没有一个能发」，而不是「选中的那个能不能发」——
// 后者在「选了 Telegram 但只配好了飞书」时会给假的否。
func (s *Service) NotifyReady() bool {
	return len(s.readyChannels()) > 0
}

// ApplyFeishu 应用一份飞书配置并确保长连接相应收敛。
//
// 网页设置保存后由 API 层调用；凭据留空即视为关闭飞书通知，
// 长连接也会一并停掉，避免拿着无效凭据反复重连刷日志。
func (s *Service) ApplyFeishu(appID, appSecret, chatID string) {
	s.fs.SetCredentials(appID, appSecret)
	s.fs.SetChatID(chatID)
	s.lc.Ensure(appID, appSecret)
}

// ApplyTelegram 应用一份 Telegram 配置并让长轮询收敛到对应状态。
//
// 只有 token 就会开始轮询（chat_id 可以为空）：用户把 bot 拉进群说句话，
// 设置页就能从「最近会话」里把 chat_id 挑出来，不必去手工翻 getUpdates 的返回。
func (s *Service) ApplyTelegram(cfg telegram.Config) {
	s.tg.SetOptions(cfg)
	s.tp.Ensure(cfg)
}

// ApplyDingTalk 应用一份钉钉配置。
//
// 只有应用凭据就会拉起 Stream 长连接（会话与模板可以后补）：
// 会话 ID 只能靠「机器人在群里收到一条消息」带上来，
// 而收消息的前提就是长连接已经在线 —— 等填完会话再连，就永远等不到。
func (s *Service) ApplyDingTalk(cfg dingtalk.Config) {
	s.dg.SetOptions(cfg)
	s.ds.Ensure(cfg)
}

// DingTalkChats 机器人在钉钉上最近收到过消息的会话。
func (s *Service) DingTalkChats() []dingtalk.ChatInfo { return s.ds.Chats() }

// dingtalkChatStore 把钉钉会话列表存进 settings 表。
//
// 存 JSON 而不是单开一张表：一条记录就 {id,title,last_at} 三个字段，
// 加表还要走 ALTER COLUMN 那套迁移，代价远大于收益。
type dingtalkChatStore struct {
	st  *store.Store
	log *slog.Logger
}

func (d dingtalkChatStore) LoadChats() []dingtalk.ChatInfo {
	if d.st == nil {
		return nil
	}
	raw, ok, err := d.st.GetSetting(settingDingTalkChats)
	if err != nil || !ok || raw == "" {
		return nil
	}
	var out []dingtalk.ChatInfo
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		// 存坏了就当没有：这份数据随时能靠再 @ 一次机器人重建，
		// 不值得为它打断启动。
		return nil
	}
	return out
}

func (d dingtalkChatStore) SaveChats(chats []dingtalk.ChatInfo) {
	if d.st == nil {
		return
	}
	b, err := json.Marshal(chats)
	if err == nil {
		err = d.st.SetSetting(settingDingTalkChats, string(b))
	}
	// 写失败只影响「重启后还在不在」，本次运行内存里那份照样能选，
	// 所以只记一条日志，不往上抛。
	if err != nil && d.log != nil {
		d.log.Warn("保存钉钉会话列表失败", "err", err)
	}
}

// DingTalkLastError 最近一次 Stream 建连失败的原因。
func (s *Service) DingTalkLastError() string { return s.ds.LastError() }

// DingTalkStreamRunning Stream 长连接的 goroutine 是否在跑。
func (s *Service) DingTalkStreamRunning() bool { return s.ds.Running() }

// ApplyWeCom 应用一份企业微信配置并让长连接收敛。
//
// 只要 BotID + Secret 就会拉起长连接（会话可以后补）：
// chatid 只能靠「机器人在群里收到一条消息」带上来，而收消息的前提
// 就是长连接已经在线 —— 等填完会话再连，就永远等不到。
func (s *Service) ApplyWeCom(cfg wecom.Config) {
	s.wc.Ensure(cfg)
}

// WeComChats 机器人在企业微信上最近收到过消息的会话。
func (s *Service) WeComChats() []wecom.ChatInfo { return s.wc.Chats() }

// WeComLastError 最近一次长连接失败的原因。
func (s *Service) WeComLastError() string { return s.wc.LastError() }

// WeComRunning 长连接的 goroutine 是否在跑。
func (s *Service) WeComRunning() bool { return s.wc.Running() }

// wecomChatStore 把企业微信会话列表存进 settings 表（同钉钉的做法）。
type wecomChatStore struct {
	st  *store.Store
	log *slog.Logger
}

func (w wecomChatStore) LoadChats() []wecom.ChatInfo {
	if w.st == nil {
		return nil
	}
	raw, ok, err := w.st.GetSetting(settingWeComChats)
	if err != nil || !ok || raw == "" {
		return nil
	}
	var out []wecom.ChatInfo
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		// 存坏了就当没有：再 @ 一次机器人就能重建，不值得为它打断启动。
		return nil
	}
	return out
}

func (w wecomChatStore) SaveChats(chats []wecom.ChatInfo) {
	if w.st == nil {
		return
	}
	b, err := json.Marshal(chats)
	if err == nil {
		err = w.st.SetSetting(settingWeComChats, string(b))
	}
	if err != nil && w.log != nil {
		w.log.Warn("保存企业微信会话列表失败", "err", err)
	}
}

// LongConnRunning 返回飞书卡片回调长连接是否已就绪。
func (s *Service) LongConnRunning() bool { return s.lc.Running() }

// TelegramPolling 返回 Telegram 按钮回调的长轮询是否在跑。
func (s *Service) TelegramPolling() bool { return s.tp.Running() }

// ChannelStatus 单个通知渠道的可用状态，供顶栏胶囊展示。
//
// Configured 说的是「写得出去」：凭据与接收会话都填了；
// Online 说的是「链路真的通」—— 不能只看轮询/长连接有没有拉起，
// 那只是 goroutine 活着，token 写错、代理不通时它照样是 true，
// 界面就会长成一副岁月静好的样子。
//
// Enabled 是用户的总开关。它和「连不上」是两件事：
// 用户主动关掉时说「未连接」会让人以为坏了，所以顶栏单独说「已暂停」。
//
// 三个渠道可能同时配好，所以这里返回列表而不是单个值：
// 顶栏只说「已连接」，具体是哪几个挂 title 上。
type ChannelStatus struct {
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	Enabled    bool   `json:"enabled"`
	Online     bool   `json:"online"`
	Error      string `json:"error,omitempty"`
}

// ChannelStatuses 列出所有填过凭据的渠道及其链路状态。
func (s *Service) ChannelStatuses() []ChannelStatus {
	out := make([]ChannelStatus, 0, 4)

	if s.fs.Configured() {
		// 飞书的 SDK 自己带重连，没有可读的错误状态，只能看长连接在不在。
		ready := s.fs.DefaultChatID() != ""
		out = append(out, ChannelStatus{
			Name:       "飞书",
			Configured: ready,
			Enabled:    s.channelEnabled(model.ChannelFeishu),
			Online:     ready && s.lc.Running(),
		})
	}
	if s.tg.Configured() {
		errText := s.tp.LastError()
		ready := s.tg.DefaultChatID() != ""
		out = append(out, ChannelStatus{
			Name:       "Telegram",
			Configured: ready,
			Enabled:    s.channelEnabled(model.ChannelTelegram),
			Online:     ready && s.tp.Running() && errText == "",
			Error:      errText,
		})
	}
	if s.dg.Configured() {
		errText := s.ds.LastError()
		// 钉钉更啰嗦一层：凭据齐只说明长连接能拉起，
		// 真要发得出卡片还得有模板和会话，缺一样都算「没配好」。
		missing := s.dg.MissingForSend()
		ready := len(missing) == 0
		errOut := errText
		if errOut == "" && !ready {
			errOut = "还缺 " + strings.Join(missing, "、")
		}
		out = append(out, ChannelStatus{
			Name:       "钉钉",
			Configured: ready,
			Enabled:    s.channelEnabled(model.ChannelDingTalk),
			Online:     ready && s.ds.Running() && errText == "",
			Error:      errOut,
		})
	}
	if s.wc.Configured() {
		errText := s.wc.LastError()
		// 与钉钉同理：凭据齐只说明长连接拉得起，发得出消息还得有接收会话。
		missing := s.wc.MissingForSend()
		ready := len(missing) == 0
		errOut := errText
		if errOut == "" && !ready {
			errOut = "还缺 " + strings.Join(missing, "、")
		}
		out = append(out, ChannelStatus{
			Name:       "企业微信",
			Configured: ready,
			Enabled:    s.channelEnabled(model.ChannelWeCom),
			Online:     ready && s.wc.Running() && errText == "",
			Error:      errOut,
		})
	}
	return out
}

// TelegramChats 最近在这个 bot 上出现过消息的会话。
func (s *Service) TelegramChats() []telegram.ChatInfo { return s.tp.Chats() }

// TelegramLastError 最近一次长轮询失败的原因。
func (s *Service) TelegramLastError() string { return s.tp.LastError() }

// Close 关闭各条按钮回调链路，用于进程退出前的收尾。
func (s *Service) Close() {
	s.lc.Stop()
	s.tp.Stop()
	s.ds.Stop()
	s.wc.Stop()
}

// recordCallback 记下一次按钮回调，返回其时间戳。
func (s *Service) recordCallback(ch model.Channel, action string) time.Time {
	now := time.Now()
	s.cbMu.Lock()
	s.cbAt, s.cbAction, s.cbChan = now, action, ch
	s.cbCount++
	s.cbMu.Unlock()
	return now
}

// CallbackState 返回最近一次按钮回调的观测结果。
// at 为零值表示服务启动以来还没收到过任何回调 —— 这正是设置页要显示的信息。
func (s *Service) CallbackState() (at time.Time, action string, ch model.Channel, count int) {
	s.cbMu.Lock()
	defer s.cbMu.Unlock()
	return s.cbAt, s.cbAction, s.cbChan, s.cbCount
}

// readyChannels 返回「开关开着、凭据填了、接收会话也有」的渠道，按飞书→Telegram→钉钉。
//
// 判 chat_id 而不是只看 Configured：应用凭据填完长连接就绿了，
// 看着像配好了，实际一条消息也发不到任何地方。
// 再叠一层开关判断：用户主动关掉的渠道不该再推新提醒。
func (s *Service) readyChannels() []model.Channel {
	out := make([]model.Channel, 0, 4)
	if s.channelEnabled(model.ChannelFeishu) && s.fs.Configured() && s.fs.DefaultChatID() != "" {
		out = append(out, model.ChannelFeishu)
	}
	if s.channelEnabled(model.ChannelTelegram) && s.tg.Configured() && s.tg.DefaultChatID() != "" {
		out = append(out, model.ChannelTelegram)
	}
	if s.channelEnabled(model.ChannelDingTalk) && s.dg.Ready() {
		out = append(out, model.ChannelDingTalk)
	}
	if s.channelEnabled(model.ChannelWeCom) && s.wc.Ready() && s.wc.Running() {
		// 企业微信多一道「长连接必须在线」：它没有 HTTP 发送接口，
		// 连接断了就一条也发不出去 —— 放着不管只会在每个渠道上刷一条发送失败。
		out = append(out, model.ChannelWeCom)
	}
	return out
}

// Notify 实现 scanner.Notifier：向所有就绪渠道发送更新提醒，返回主投递的消息 ID。
//
// 一个渠道失败不影响另一个：两边都配了却只收到一条，也比一条都没收到强。
// 只有**全部**失败才返回错误，让扫描器把失败原因记进日志。
func (s *Service) Notify(ctx context.Context, n *model.Notification) (string, error) {
	chans := s.readyChannels()
	if len(chans) == 0 {
		n.Channel = ""
		s.log.Debug("未配置通知渠道，提醒仅落库", "ref", n.ImageRef)
		return "", nil
	}

	var (
		deliveries []model.Delivery
		failures   []string
	)

	for _, ch := range chans {
		switch ch {
		case model.ChannelTelegram:
			chatID := s.tg.DefaultChatID()
			msgID, err := s.tg.SendMessage(ctx, chatID, telegram.BuildUpdateMessage(n))
			if err != nil {
				failures = append(failures, "Telegram："+err.Error())
				s.log.Warn("Telegram 推送失败", "ref", n.ImageRef, "err", err)
				continue
			}
			deliveries = append(deliveries, model.Delivery{
				Channel:   model.ChannelTelegram,
				ChatRef:   chatID,
				MessageID: strconv.FormatInt(msgID, 10),
			})
			s.log.Info("已推送更新提醒", "channel", "telegram", "ref", n.ImageRef, "message_id", msgID)

		case model.ChannelDingTalk:
			conv := s.dg.ConversationID()
			outTrack := outTrackID(n.ID)
			if err := s.dg.CreateAndDeliverCard(ctx, outTrack, dingtalk.BuildUpdateParams(n)); err != nil {
				failures = append(failures, "钉钉："+err.Error())
				s.log.Warn("钉钉推送失败", "ref", n.ImageRef, "err", err)
				continue
			}
			// 钉钉的「消息坐标」就是 outTrackId：更新卡片、定位按钮回调都靠它，
			// 不像飞书/Telegram 那样另有一个 message_id。
			deliveries = append(deliveries, model.Delivery{
				Channel:   model.ChannelDingTalk,
				ChatRef:   conv,
				MessageID: outTrack,
			})
			s.log.Info("已推送更新提醒", "channel", "dingtalk", "ref", n.ImageRef, "out_track_id", outTrack)

		case model.ChannelFeishu:
			chatID := s.fs.DefaultChatID()
			msgID, err := s.fs.SendCard(ctx, "", feishu.BuildUpdateCard(n))
			if err != nil {
				failures = append(failures, "飞书："+err.Error())
				s.log.Warn("飞书推送失败", "ref", n.ImageRef, "err", err)
				continue
			}
			deliveries = append(deliveries, model.Delivery{
				Channel:   model.ChannelFeishu,
				ChatRef:   chatID,
				MessageID: msgID,
			})
			s.log.Info("已推送更新提醒", "channel", "feishu", "ref", n.ImageRef, "message_id", msgID)

		case model.ChannelWeCom:
			taskID := outTrackID(n.ID)
			card := wecom.BuildUpdateCard(n, taskID)
			if err := s.wc.SendTemplateCard(ctx, card); err != nil {
				failures = append(failures, "企业微信："+err.Error())
				s.log.Warn("企业微信推送失败", "ref", n.ImageRef, "err", err)
				continue
			}
			// 企业微信的「消息坐标」就是 task_id：按钮回调里带的是它，
			// 没有别的字段能把一条回调对回某条通知。
			deliveries = append(deliveries, model.Delivery{
				Channel:   model.ChannelWeCom,
				ChatRef:   s.wc.ChatID(),
				MessageID: taskID,
			})
			s.log.Info("已推送更新提醒", "channel", "wecom", "ref", n.ImageRef, "task_id", taskID)
		}
	}

	if len(deliveries) == 0 {
		return "", errors.New("所有通知渠道都发送失败：" + strings.Join(failures, "；"))
	}
	if len(failures) > 0 {
		s.log.Warn("部分渠道推送失败", "ref", n.ImageRef, "failures", strings.Join(failures, "；"))
	}

	// 主投递 = 第一条发成功的，兼容只认单渠道的老代码与前端展示。
	n.Deliveries = deliveries
	n.Channel = deliveries[0].Channel
	n.ChatRef = deliveries[0].ChatRef
	n.MessageID = deliveries[0].MessageID
	return deliveries[0].MessageID, nil
}

// handleCardAction 实现 feishu.ActionHandler：处理飞书卡片按钮点击。
//
// 必须在 3 秒内返回，因此这里只创建任务并立刻返回「进行中」的卡片，
// 真正的重建放到后台 goroutine 里跑。
func (s *Service) HandleCardAction(ctx context.Context, v feishu.CardValue) (string, map[string]any) {
	at := s.recordCallback(model.ChannelFeishu, v.Action)

	// 测试卡片上的按钮不关联任何通知，必须在查库**之前**处理，
	// 否则会掉进下面「通知已失效」那条分支 —— 表现为点了按钮提示
	// 「通知已失效」，让人以为是自己配错了。
	if v.Action == ActionTest {
		s.log.Info("收到飞书测试卡片回调，按钮链路正常")
		return "回调已收到", feishu.BuildTestDoneCard(at)
	}

	n, err := s.st.GetNotification(v.NID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "通知已失效", nil
		}
		s.log.Warn("读取通知失败", "nid", v.NID, "err", err)
		return "服务异常", nil
	}

	out := s.applyAction(n, v.Action, model.ChannelFeishu)
	if out.State == "" {
		return out.Toast, nil
	}
	return out.Toast, feishu.BuildStateCard(n, out.State, out.Detail)
}

// HandleCallback 实现 telegram.ActionHandler：处理按钮点击。
//
// 返回值：toast 用于回应客户端（不回按钮会一直转圈），replace 用于原地改消息。
func (s *Service) HandleCallback(ctx context.Context, v telegram.CallbackValue, chatID string, messageID int64) (string, *telegram.Message) {
	at := s.recordCallback(model.ChannelTelegram, v.Action)

	if v.Action == ActionTest {
		s.log.Info("收到 Telegram 测试消息回调，按钮链路正常")
		m := telegram.BuildTestDoneMessage(at)
		return "回调已收到", &m
	}

	n, err := s.st.GetNotification(v.NID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "通知已失效", nil
		}
		s.log.Warn("读取通知失败", "nid", v.NID, "err", err)
		return "服务异常", nil
	}

	// 按钮所在会话才是权威坐标：通知是按默认会话发的，
	// 但同一条消息被转发到别的群后，点按钮拿到的是转发后那个会话。
	//
	// 只改 Telegram 这一份投递记录 —— 主投递很可能是飞书的 chip，
	// 顺手覆盖它会把飞书的会话 ID 写成 Telegram 的 chat_id。
	if chatID != "" {
		s.fixTelegramChatRef(n, chatID)
	}

	out := s.applyAction(n, v.Action, model.ChannelTelegram)
	if out.State == "" {
		return out.Toast, nil
	}
	m := telegram.BuildStateMessage(n, out.State, out.Detail)
	return out.Toast, &m
}

// fixTelegramChatRef 用「点按钮时所在的会话」纠正 Telegram 那份投递坐标。
func (s *Service) fixTelegramChatRef(n *model.Notification, chatID string) {
	if len(n.Deliveries) == 0 {
		// 老库：只有主投递一份，且它必然是 Telegram 的（1.2.4 之前没有多渠道路径）。
		if n.ChatRef == chatID {
			return
		}
		if err := s.st.SetNotificationChatRef(n.ID, chatID); err != nil {
			s.log.Warn("记录 Telegram 会话失败", "nid", n.ID, "err", err)
		}
		n.ChatRef = chatID
		return
	}

	changed := false
	for i := range n.Deliveries {
		if n.Deliveries[i].Channel == model.ChannelTelegram && n.Deliveries[i].ChatRef != chatID {
			n.Deliveries[i].ChatRef = chatID
			changed = true
		}
	}
	if !changed {
		return
	}
	if err := s.st.UpdateNotificationDeliveries(n.ID, n.Deliveries); err != nil {
		s.log.Warn("记录 Telegram 会话失败", "nid", n.ID, "err", err)
	}
	if n.Channel == model.ChannelTelegram {
		n.ChatRef = chatID
	}
}

// ---- 钉钉 ----

// outTrackSeq 卡片 ID 的进程内自增序号。
//
// 单靠纳秒时间戳不够：Windows 上 time.Now() 的精度只有百纳秒量级，
// 两次相邻调用拿到同一个值很常见。而钉钉按 outTrackId 唯一标识一张卡，
// 撞了第二张就投放不进去，报 cardAlreadyExist —— 现象是「发了两条通知，只收到一张卡」。
var outTrackSeq atomic.Uint64

// outTrackID 生成卡片实例 ID。
//
// 里面塞了通知 ID：钉钉的按钮回传参数由卡片模板定义，没有地方能像飞书那样
// 把 nid 一起塞进按钮 value 里，所以只能靠卡片 ID 反查是哪条通知。
func outTrackID(nid int64) string {
	return fmt.Sprintf("ad-%d-%d-%d", nid, time.Now().UnixNano(), outTrackSeq.Add(1))
}

// outTrackIsTest 判断这张卡片是不是「发送测试」发的。
//
// 测试卡片用的是 nid=0，所以 ID 形如 ad-0-<纳秒>-<序号>。
// 前缀比对就够了：真正的通知 nid 恒大于 0（见 parseOutTrackID 的校验）。
func outTrackIsTest(s string) bool {
	return strings.HasPrefix(s, "ad-0-")
}

// parseOutTrackID 从卡片 ID 里取回通知 ID，格式不对返回 0。
func parseOutTrackID(s string) int64 {
	parts := strings.Split(s, "-")
	if len(parts) < 3 || parts[0] != "ad" {
		return 0
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// HandleDingTalkCard 实现 dingtalk.Handler：处理钉钉卡片按钮点击。
//
// 返回值随 ACK 立刻写回卡片 —— 钉钉那边按钮点下去，卡片变不变样全看这个。
// 事后再异步调一次更新接口是给群里其他人看的（官方明说 ACK 里的
// cardData 只下发给触发这次点击的那个人）。
func (s *Service) HandleDingTalkCard(outTrack, action string) map[string]string {
	at := s.recordCallback(model.ChannelDingTalk, action)

	// 测试卡片不关联任何通知，必须在查库之前处理（它没有对应的通知记录）。
	//
	// 它的卡片 ID 是 ad-0-…，模板里那三个业务按钮点哪个都会走到这里：
	// 用户不会去分辨「该点哪个」，任何一下点击都应该算把按钮链路验通了。
	// 只认 action=="test" 的话，用户点「立即更新」会毫无反应，看着像链路坏了。
	if action == ActionTest || outTrackIsTest(outTrack) {
		s.log.Info("收到钉钉测试卡片回调，按钮链路正常", "action", action)
		done := dingtalk.BuildTestDoneParams(at.Format("2006-01-02 15:04:05"))
		s.updateDingTalkCardAsync(outTrack, done)
		return done
	}

	nid := parseOutTrackID(outTrack)
	if nid <= 0 {
		s.log.Warn("钉钉卡片回调里没有可识别的通知 ID", "out_track_id", outTrack)
		return nil
	}
	n, err := s.st.GetNotification(nid)
	if err != nil {
		// 查不到也要说一声：这条分支以前是静默的，用户点完没反应、
		// 日志里又什么都没有，只能靠猜。
		s.log.Warn("钉钉卡片回调指向的通知不存在", "nid", nid, "out_track_id", outTrack, "err", err)
		return nil
	}
	// 核对这张卡片确实是这条通知发出去的。
	// outTrackId 的格式是可推导的，不校验就等于「任何人构造一个 ID 就能
	// 让任意一条历史通知触发容器重建」—— 重建容器等同于系统最高权限。
	if !s.hasDingTalkDelivery(n, outTrack) {
		s.log.Warn("钉钉卡片回调与投递记录不匹配，已忽略", "nid", nid, "out_track_id", outTrack)
		return nil
	}

	out := s.applyAction(n, action, model.ChannelDingTalk)
	if out.State == "" {
		// 「更新已在进行中」「未知操作」两种情况：状态没变，也就没有新卡片数据可回。
		// 但必须留一行，否则现象和「回调根本没进来」一模一样。
		s.log.Warn("钉钉卡片回调未改变任何状态",
			"nid", nid, "action", action, "card_state", n.CardState, "toast", out.Toast)
		return nil
	}
	params := dingtalk.BuildStateParams(n, out.State, out.Detail)
	s.updateDingTalkCardAsync(outTrack, params)
	return params
}

// hasDingTalkDelivery 这条通知是否真的投递到了指定卡片实例。
func (s *Service) hasDingTalkDelivery(n *model.Notification, outTrack string) bool {
	for _, d := range n.Deliveries {
		if d.Channel == model.ChannelDingTalk && d.MessageID == outTrack {
			return true
		}
	}
	// 老数据没有 deliveries 时退回主投递那一份
	return len(n.Deliveries) == 0 &&
		n.Channel == model.ChannelDingTalk && n.MessageID == outTrack
}

// updateDingTalkCardAsync 异步改写钉钉卡片。
//
// 同步做会拖长 ACK 时间：UpdateCard 前面可能还有一次 accessToken 请求，
// 两跳加起来足够超时，表现为「点了按钮，卡片一直没变化」。
func (s *Service) updateDingTalkCardAsync(outTrack string, params dingtalk.CardParams) {
	if outTrack == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(s.baseCtx, 20*time.Second)
		defer cancel()
		if err := s.dg.UpdateCard(ctx, outTrack, params); err != nil {
			s.log.Warn("更新钉钉卡片失败", "out_track_id", outTrack, "err", err)
		}
	}()
}

// HandleBotMessage 实现 dingtalk.Handler：机器人在某个会话里收到了消息。
//
// 故意不回任何内容。这条消息的唯一作用是让钉钉把 conversationId 带上来，
// 用户不必去后台手抄一串 cid 开头的 ID；回一句「收到」只会在群里刷屏。
func (s *Service) HandleDingTalkMessage(conversationID, title string) {
	s.log.Info("钉钉机器人收到消息，已记录会话", "conversation_id", conversationID, "title", title)
}

// SendTestDingTalk 投放一张带按钮的测试卡片。
//
// 与飞书同一个道理：卡片发得出去，不代表按钮点得动 ——
// 发卡片走服务端 API，按钮点击走 Stream 长连接，是两条独立链路。
func (s *Service) SendTestDingTalk(ctx context.Context) error {
	if !s.dg.Configured() {
		return errors.New("钉钉应用 Client ID / Client Secret 未配置")
	}
	if miss := s.dg.MissingForSend(); len(miss) > 0 {
		return errors.New("还缺 " + strings.Join(miss, "、") + "，请先补齐再测试")
	}
	return s.dg.CreateAndDeliverCard(ctx, outTrackID(0), dingtalk.BuildTestParams(s.dg.ConversationID()))
}

// ---- 企业微信 ----

// HandleWeComCard 实现 wecom.Handler：处理模板卡片按钮点击。
//
// 返回值会被立刻写回卡片 —— 企业微信只给 5 秒，且只能用这次事件的 req_id。
// 超过 5 秒的回写会被服务端丢弃，用户看到的就只是「点了没反应」。
func (s *Service) HandleWeComCard(ev wecom.CardEvent) *wecom.TemplateCard {
	at := s.recordCallback(model.ChannelWeCom, ev.Action)

	// 「知道了」只是把按钮点掉。状态卡片不带业务按钮，但卡片类型要求至少一个，
	// 于是有了这个按钮 —— 点它不该改任何状态，也不回写卡片（回了也是同一张）。
	if ev.Action == wecom.ActionAck {
		return nil
	}

	// 通知 ID 有两条来源。task_id 是官方的正规途径，但主动推送（aibot_send_msg）
	// 的卡片被点击时它实测为空；按钮 key 里另带了一份，用作兜底。
	nid := parseOutTrackID(ev.TaskID)
	if nid <= 0 && ev.NotifID > 0 {
		nid = ev.NotifID
	}

	// 测试卡片不关联任何通知，必须在查库之前处理。
	// 它有两个特征：task_id 形如 ad-0-…，或按钮 key 里的通知 ID 明确写着 0。
	// 三个业务按钮点哪个都算把链路验通了。
	if outTrackIsTest(ev.TaskID) || (ev.NotifID == 0 && ev.Action != "") {
		s.log.Info("收到企业微信测试卡片回调，按钮链路正常", "action", ev.Action)
		card := wecom.BuildTestDoneCard(ev.TaskID, at.Format("2006-01-02 15:04:05"))
		return &card
	}

	if nid <= 0 {
		s.log.Warn("企业微信卡片回调里没有可识别的通知 ID",
			"task_id", ev.TaskID, "event_key", ev.EventKey)
		return nil
	}
	n, err := s.st.GetNotification(nid)
	if err != nil {
		s.log.Warn("企业微信卡片回调指向的通知不存在", "nid", nid, "task_id", ev.TaskID, "err", err)
		return nil
	}
	// 核对这张卡片确实是这条通知发出去的：task_id 的格式是可推导的，
	// 不校验就等于「任何人构造一个 ID 就能触发任意一条历史通知重建容器」。
	if !s.hasWeComDelivery(n, ev.TaskID) {
		s.log.Warn("企业微信卡片回调与投递记录不匹配，已忽略", "nid", nid, "task_id", ev.TaskID)
		return nil
	}

	out := s.applyAction(n, ev.Action, model.ChannelWeCom)
	if out.State == "" {
		// 「更新已在进行中」「未知操作」：状态没变，也就没有新卡片可回。
		// 但必须留一行日志，否则现象和「回调根本没进来」一模一样。
		s.log.Warn("企业微信卡片回调未改变任何状态",
			"nid", nid, "action", ev.Action, "event_key", ev.EventKey,
			"card_state", n.CardState, "toast", out.Toast)
		return nil
	}
	card := wecom.BuildStateCard(n, ev.TaskID, out.State, out.Detail)
	return &card
}

// HandleWeComSession 实现 wecom.Handler：机器人在某个会话里收到了消息。
//
// 故意不回任何内容：这条消息的唯一作用是让企业微信把 chatid 带上来，
// 用户不必去后台手抄一串 ID。回一句「收到」只会在群里刷屏。
func (s *Service) HandleWeComSession(chatID, chatType, userID string) {
	s.log.Info("企业微信机器人收到消息，已记录会话",
		"chat_id", chatID, "chat_type", chatType, "user_id", userID)
}

// hasWeComDelivery 这条通知是否真的投递到了企业微信。
//
// taskID 非空时必须逐条对上：卡片 ID 的格式是可推导的，不校验就等于
// 「构造一个 ID 就能触发任意一条历史通知重建容器」。
//
// taskID 为空时（主动推送的卡片被点击时实测如此）退回「这条通知投递过企业微信」：
// 此时唯一的凭据是按钮 key 里的通知 ID，而 key 同样由本程序生成，不是外部可猜的输入。
func (s *Service) hasWeComDelivery(n *model.Notification, taskID string) bool {
	for _, d := range n.Deliveries {
		if d.Channel != model.ChannelWeCom {
			continue
		}
		if taskID == "" || d.MessageID == taskID {
			return true
		}
	}
	return len(n.Deliveries) == 0 && n.Channel == model.ChannelWeCom &&
		(taskID == "" || n.MessageID == taskID)
}

// SendTestWeCom 推一张带按钮的测试卡片。
//
// 与飞书 / 钉钉同一个道理：卡片发得出去不代表按钮点得动 ——
// 发卡片走 aibot_send_msg，按钮点击走 aibot_event_callback，两条独立链路。
func (s *Service) SendTestWeCom(ctx context.Context) error {
	if !s.wc.Configured() {
		return errors.New("企业微信 BotID / Secret 未配置")
	}
	// 先看连接：企业微信所有发送都要经过长连接，没连上时 SendTemplateCard
	// 只会回一句「长连接未建立」，用户不知道该等一等还是去查凭据。
	if !s.wc.Running() {
		if msg := s.wc.LastError(); msg != "" {
			return fmt.Errorf("长连接未建立：%s", msg)
		}
		return errors.New("长连接还在建立中，请稍等几秒再试")
	}
	if miss := s.wc.MissingForSend(); len(miss) > 0 {
		return errors.New("还缺 " + strings.Join(miss, "、") + "，请先补齐再测试")
	}
	return s.wc.SendTemplateCard(ctx, wecom.BuildTestCard(outTrackID(0)))
}

// actionOutcome 一次按钮点击的处理结果（与具体渠道无关）。
type actionOutcome struct {
	Toast  string
	State  model.CardState // 空表示不需要改写原消息
	Detail string
}

// applyAction 是所有渠道（飞书 / Telegram / 将来的其它）共用的落地逻辑。
//
// 带上 from（哪个渠道点的）：主投递恒为飞书，用它来记「谁触发的更新」
// 会让 Telegram 里点的按钮在历史里显示成飞书点的。
func (s *Service) applyAction(n *model.Notification, action string, from model.Channel) actionOutcome {
	switch action {
	case ActionIgnore:
		if err := s.st.SetNotificationCardState(n.ID, model.CardIgnored); err != nil {
			s.log.Warn("更新通知状态失败", "err", err)
		}
		_ = s.st.MarkNotificationRead(n.ID, true)
		// 光把这条通知标成「已忽略」是拦不住下一条消息的：扫描只看远端摘要变没变，
		// 下一轮照样判成「有更新」并新建一条通知。必须把「用户已放行这一版」
		// 记到监控项上，消息里那句「同一个版本不会再次提醒」才是真的。
		s.rememberIgnoredDigest(n)
		return actionOutcome{
			Toast:  "已忽略该版本",
			State:  model.CardIgnored,
			Detail: "已忽略本版本，同一个版本不会再次提醒。\n后续出现更高的版本时仍会通知。",
		}

	case ActionSnooze:
		if err := s.st.SetNotificationCardState(n.ID, model.CardSnoozed); err != nil {
			s.log.Warn("更新通知状态失败", "err", err)
		}
		return actionOutcome{
			Toast:  "已设为稍后",
			State:  model.CardSnoozed,
			Detail: "已记录。本轮巡检不会重复提醒，下次巡检如有更新会重新推送。",
		}

	case ActionUpdate:
		if n.CardState == model.CardUpdating {
			// 重复点击：不再起第二个任务，但要把卡片刷成当前状态。
			// 早先这里返回的 State 是空的，于是一连点两下就是「两下都没反应」，
			// 用户会以为第一次也没生效。
			return actionOutcome{
				Toast:  "更新已在进行中",
				State:  model.CardUpdating,
				Detail: "这条更新的任务已经在跑了，完成后这里会自动变成结果，不用重复点。",
			}
		}
		taskID, err := s.TriggerUpdate(n, string(from))
		if err != nil {
			return actionOutcome{Toast: "无法更新", State: model.CardFailed, Detail: err.Error()}
		}
		return actionOutcome{
			Toast: "已开始更新",
			State: model.CardUpdating,
			Detail: fmt.Sprintf(
				"已受理，正在拉取镜像并重建容器（任务 #%d）。\n完成后本条消息会自动更新结果，期间无需其他操作。", taskID),
		}

	default:
		return actionOutcome{Toast: "未知操作"}
	}
}

// rememberIgnoredDigest 把「用户放行的这次远端内容」记到监控项上。
//
// 记的是库里**当前**的远端摘要，而不是通知里那份快照：用户点按钮时想放行的是
// 「现在仓库里这一版」，以最新观测为准更稳妥（期间要是又推了一版，下一轮该提醒还得提醒）。
// 记录失败不影响回调返回 —— 按钮已经点了，消息状态也改了，这里只是尽力而为，
// 真失败了顶多下次再被提醒一遍。
func (s *Service) rememberIgnoredDigest(n *model.Notification) {
	if n.ContainerID == "" {
		return
	}
	img, err := s.st.FindImage(n.ImageRef, n.ContainerID)
	if err != nil {
		s.log.Warn("读取监控项失败，无法记录已忽略版本", "ref", n.ImageRef, "err", err)
		return
	}
	if img.RemoteDigest == "" {
		return
	}
	if err := s.st.SetImageIgnoredDigest(img.ID, img.RemoteDigest); err != nil {
		s.log.Warn("记录已忽略版本失败", "ref", n.ImageRef, "err", err)
	}
}

// TriggerUpdate 创建并异步执行一次更新任务，返回任务 ID。
// 三个入口（飞书按钮 / Telegram 按钮 / Web 页面）共用这一处，行为才保证一致。
func (s *Service) TriggerUpdate(n *model.Notification, triggeredBy string) (int64, error) {
	if n.ContainerID == "" {
		return 0, errors.New("该监控项没有关联的本地容器，无法执行更新")
	}
	if s.up.Busy() {
		return 0, errors.New("已有更新任务正在执行，请稍后重试")
	}
	if busy, err := s.st.HasRunningTask(n.ContainerID); err == nil && busy {
		return 0, errors.New("该容器已有进行中的更新任务")
	}

	task := &model.Task{
		NotificationID: n.ID,
		ImageRef:       s.updateTarget(n),
		ContainerID:    n.ContainerID,
		ContainerName:  n.ContainerName,
		TriggeredBy:    triggeredBy,
	}
	taskID, err := s.st.CreateTask(task)
	if err != nil {
		s.log.Error("创建更新任务失败", "err", err)
		return 0, fmt.Errorf("创建更新任务失败: %w", err)
	}
	if n.ID > 0 {
		if err := s.st.SetNotificationCardState(n.ID, model.CardUpdating); err != nil {
			s.log.Warn("更新通知状态失败", "err", err)
		}
	}

	go s.runUpdate(n, taskID, triggeredBy)
	return taskID, nil
}

// runUpdate 在后台执行更新，并把最终结果写回原消息。
func (s *Service) runUpdate(n *model.Notification, taskID int64, triggeredBy string) {
	ctx, cancel := context.WithTimeout(s.baseCtx, 45*time.Minute)
	defer cancel()

	target := s.updateTarget(n)
	s.log.Info("开始执行更新", "task", taskID, "container", n.ContainerName,
		"from", n.ImageRef, "target", target, "by", triggeredBy)

	err := s.up.Run(ctx, updater.Request{
		TaskID:        taskID,
		ImageRef:      target,
		ContainerID:   n.ContainerID,
		ContainerName: n.ContainerName,
		TriggeredBy:   triggeredBy,
	})

	state := model.CardDone
	detail := "容器已在新镜像上重建完成，原有挂载、端口与网络配置保持不变。"
	if err != nil {
		state = model.CardFailed
		detail = "更新失败：" + err.Error()
		if t, terr := s.st.GetTask(taskID); terr == nil && t.Status == model.TaskRolledBack {
			state = model.CardRollback
			detail = "新容器未通过健康检查，已自动回滚到原版本（原容器已恢复运行）。\n原因：" + err.Error()
		}
	}

	if n.ID > 0 {
		if serr := s.st.SetNotificationCardState(n.ID, state); serr != nil {
			s.log.Warn("写回通知状态失败", "err", serr)
		}
		_ = s.st.MarkNotificationRead(n.ID, true)
	}

	// 原地替换原消息，形成可追溯的操作记录。
	// 两个渠道各发过一份，就必须两条都改 —— 漏掉的那条会永远停在「待更新」，
	// 用户点进去还以为是没生效。
	targets := s.deliveryTargets(n)
	if len(targets) == 0 {
		return
	}
	pushCtx, cancelPush := context.WithTimeout(s.baseCtx, 30*time.Second)
	defer cancelPush()
	for _, d := range targets {
		s.editDelivery(pushCtx, n, d, state, detail)
	}
}

// deliveryTargets 取这条通知的全部投递坐标。
//
// 老库（1.2.4 之前建的记录）没有 deliveries，退回主投递的那一份。
func (s *Service) deliveryTargets(n *model.Notification) []model.Delivery {
	if len(n.Deliveries) > 0 {
		return n.Deliveries
	}
	if n.MessageID == "" {
		return nil
	}
	return []model.Delivery{{Channel: n.Channel, ChatRef: n.ChatRef, MessageID: n.MessageID}}
}

// editDelivery 把某个渠道上那张卡片/消息改成最终状态。
//
// 这里只看「凭据还在不在」，不看开关：用户中途关掉渠道时，
// 之前已经发出去的那张卡片仍然要改成「已完成」，
// 否则它会永远停在「待更新」，看起来像更新根本没成功。
func (s *Service) editDelivery(ctx context.Context, n *model.Notification, d model.Delivery, state model.CardState, detail string) {
	switch d.Channel {
	case model.ChannelTelegram:
		if !s.tg.Configured() {
			return
		}
		chatID := d.ChatRef
		if chatID == "" {
			chatID = s.tg.DefaultChatID()
		}
		msgID, err := strconv.ParseInt(d.MessageID, 10, 64)
		if err != nil {
			s.log.Warn("Telegram 消息 ID 非法", "message_id", d.MessageID, "err", err)
			return
		}
		if err := s.tg.EditMessageText(ctx, chatID, msgID, telegram.BuildStateMessage(n, state, detail)); err != nil {
			s.log.Warn("更新 Telegram 消息失败", "message_id", d.MessageID, "err", err)
		}

	case model.ChannelDingTalk:
		if !s.dg.Configured() {
			return
		}
		if err := s.dg.UpdateCard(ctx, d.MessageID, dingtalk.BuildStateParams(n, state, detail)); err != nil {
			s.log.Warn("更新钉钉卡片失败", "out_track_id", d.MessageID, "err", err)
		}

	case model.ChannelWeCom:
		// 企业微信只能「补发一条」，不能改原来那张卡：卡片更新必须用按钮事件的
		// req_id，而那个窗口只有 5 秒，重建容器早就超时了。
		//
		// 原卡片会停在「正在更新」，它的副标题里已经写明「完成后会另发一条消息」，
		// 所以用户不会以为卡住了。
		if !s.wc.Ready() || !s.wc.Running() {
			return
		}
		if err := s.wc.SendMarkdown(ctx, wecom.BuildDoneMessage(n, state, detail)); err != nil {
			s.log.Warn("发送企业微信结果消息失败", "ref", n.ImageRef, "err", err)
		}

	default:
		if !s.fs.Configured() {
			return
		}
		if err := s.fs.PatchCard(ctx, d.MessageID, feishu.BuildStateCard(n, state, detail)); err != nil {
			s.log.Warn("更新飞书卡片失败", "message_id", d.MessageID, "err", err)
		}
	}
}

// SendTestCard 发送一张飞书测试卡片，用于部署后验证配置。
func (s *Service) SendTestCard(ctx context.Context) error {
	if !s.fs.Configured() {
		return errors.New("飞书应用未配置，请先填写 App ID 与 App Secret")
	}
	chatID := s.fs.DefaultChatID()
	if chatID == "" {
		return errors.New("尚未配置接收群 chat_id")
	}
	_, err := s.fs.SendCard(ctx, chatID, feishu.BuildTestCard(chatID))
	return err
}

// SendTestTelegram 发送一条带按钮的 Telegram 测试消息。
//
// 与飞书同一个道理：能收到消息不代表按钮点得动，所以测试消息必须带按钮，
// 点一下、原地变成回执，才算把「发消息 + 收回调」整条链路验完。
func (s *Service) SendTestTelegram(ctx context.Context) error {
	if !s.tg.Configured() {
		return errors.New("Telegram Bot Token 未配置")
	}
	// 先确认 token 有效、网络通：token 错了 sendMessage 的报错是「unauthorized」，
	// 看着像聊天 ID 的问题，先跑一遍 getMe 能把结论说清楚。
	// 顺序很重要 —— 如果先检查 chat_id，网络不通的用户会先看到「没有会话」，
	// 于是回去折腾会话 ID，而真正的问题在出网上。
	if name, err := s.tg.GetMe(ctx); err != nil {
		return fmt.Errorf("Bot Token 校验失败: %w", err)
	} else if name != "" {
		s.log.Info("Telegram bot 校验通过", "bot", name)
	}
	chatID := s.tg.DefaultChatID()
	if chatID == "" {
		return errors.New("尚未配置接收会话 chat_id（把 bot 拉进群 @它 说句话，点「读取会话」）")
	}
	_, err := s.tg.SendMessage(ctx, chatID, telegram.BuildTestMessage(chatID))
	return err
}

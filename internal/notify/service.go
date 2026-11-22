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
	"html"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/haihaipypy/autodocker/internal/hubproto"
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

	// 远端节点更新能力（hubserver）。为 nil 时带节点的提醒只能看不能点。
	remoteMu sync.RWMutex
	remote   RemoteNodes

	// 合并提醒的「一键全更新」同一时刻只允许一个在跑。
	//
	// 底层更新器本来就是单飞的（本机一把锁、远端一台机器一个任务），
	// 同时起两批只会在第一个成员就撞「已有更新任务正在执行」，
	// 用户看到的是「点了没反应还报错」。串起来，第二批等第一批跑完再开始。
	batchMu sync.Mutex
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

// RemoteNodes 远端节点的更新能力，由 hubserver 实现。
//
// 用接口而不是直接依赖 hubserver：通知服务不该知道 WebSocket、令牌、快照
// 这些东西，它只需要「这台机器能不能动」和「让它动起来」两件事。
type RemoteNodes interface {
	Online(nodeID int64) bool
	// UpdateContainer 让那台机器重建一个容器，返回受理说明与实际下发的容器快照。
	//
	// 快照要带回来：容器被重建过时提醒里的编号已经过期，服务端按名字找回了
	// 新编号，这边得把它写回通知，否则节点回传结果时对不上那一条卡片。
	UpdateContainer(nodeID int64, containerID, containerName string) (string, *model.NodeContainer, error)
	// PullContainer 只让节点把镜像拉下来，不重建容器。
	//
	// 批量更新的第一步：整批镜像先拉齐，再动容器。旧 Agent 会回
	// 「不支持的指令」，调用方识别后退化成逐个更新，不算失败。
	PullContainer(nodeID int64, containerID, containerName string) error
	// OpResult 最近一次操作到了哪一步、耗时多久。批量更新用它判断一个成员
	// 是否已经跑完，以及是成功还是失败。
	OpResult(nodeID int64) (phase, message string, updatedAt time.Time)
	// Busy 那台机器此刻是不是真有一个更新在跑。
	//
	// 用来分辨「卡片停在更新中」到底是真在跑，还是任务早已结束、只是状态没写回来
	// （进程重启、事件帧丢了，或者旧版本留下的僵尸状态）。分不清的话，
	// 那种卡片再点一次只会永远回「进行中」，用户没有任何办法收尾。
	Busy(nodeID int64) bool
}

// SetRemoteNodes 注入远端更新能力。不注入时，带 node_id 的提醒只能看不能点。
func (s *Service) SetRemoteNodes(r RemoteNodes) {
	s.remoteMu.Lock()
	s.remote = r
	s.remoteMu.Unlock()
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

// PushRequest 外部应用交代要推的一条消息。
type PushRequest struct {
	Title   string
	Body    string
	Channel string // 空或 all 表示所有已就绪渠道
}

// PushResult 单个渠道的投递结果。
type PushResult struct {
	Channel string `json:"channel"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// Push 把一条外部消息推到已就绪的渠道，逐渠道返回结果。
//
// 与 Notify 走的不是同一条路，这点是有意的：这里不落库、不建通知记录、
// 不带任何按钮。外部消息没有「哪个容器要更新」这回事，硬套进那套模型
// 只会让页面的历史列表混进一堆点不动的条目。
//
// 渠道之间互不影响，一个发出去就算成功；全部失败时才返回错误。
func (s *Service) Push(ctx context.Context, in PushRequest) ([]PushResult, error) {
	chans, err := s.pushTargets(in.Channel)
	if err != nil {
		return nil, err
	}

	title := strings.TrimSpace(in.Title)
	body := strings.TrimSpace(in.Body)

	out := make([]PushResult, 0, len(chans))
	failed := 0
	for _, ch := range chans {
		res := PushResult{Channel: string(ch)}
		var serr error
		switch ch {
		case model.ChannelFeishu:
			_, serr = s.fs.SendText(ctx, s.fs.DefaultChatID(), plainMessage(title, body))
		case model.ChannelTelegram:
			// Telegram 那边 parse_mode 是 HTML，正文必须转义：
			// 用户消息里出现 < 或 & 会让接口直接报 400，而报错只说「can't parse entities」。
			_, serr = s.tg.SendMessage(ctx, s.tg.DefaultChatID(),
				telegram.Message{Text: htmlMessage(title, body)})
		case model.ChannelDingTalk:
			serr = s.dg.CreateAndDeliverCard(ctx, outTrackID(0), dingtalk.BuildPushParams(title, body))
		case model.ChannelWeCom:
			serr = s.wc.SendMarkdown(ctx, markdownMessage(title, body))
		}
		res.OK = serr == nil
		if serr != nil {
			res.Error = serr.Error()
			failed++
			s.log.Warn("外部推送失败", "channel", string(ch), "title", title, "err", serr)
		}
		out = append(out, res)
	}

	if failed == len(out) {
		return out, errors.New("所有渠道都没能发出去")
	}
	return out, nil
}

// PushTargetError 渠道选择有问题：调用方指了个不存在的渠道，或者指的那个没就绪。
//
// 与「渠道都试过但都没发出去」分开：这是调用方看得懂、也改得动的问题，
// 接口应当回 400，而不是笼统的 502 让人以为服务端挂了。
type PushTargetError struct{ msg string }

func (e *PushTargetError) Error() string { return e.msg }

func pushTargetErr(msg string) error { return &PushTargetError{msg: msg} }

// pushTargets 决定这次推到哪些渠道。
func (s *Service) pushTargets(want string) ([]model.Channel, error) {
	return resolvePushChannel(want, s.readyChannels())
}

// resolvePushChannel 把调用方要的渠道映射成实际要发的那一串。
//
// 指名要某个渠道却没就绪时**直接报错**，不退回「发到别的渠道」：
// 调用方看到成功就会以为消息到了它想去的那个群，错发比发不出去更麻烦。
func resolvePushChannel(want string, ready []model.Channel) ([]model.Channel, error) {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" || want == "all" {
		if len(ready) == 0 {
			return nil, pushTargetErr("还没有已就绪的通知渠道：请先在设置页配好并打开开关")
		}
		return ready, nil
	}
	ch := model.Channel(want)
	if !ch.Valid() {
		return nil, pushTargetErr(fmt.Sprintf("不认识的渠道 %q，可用：all / feishu / telegram / dingtalk / wecom", want))
	}
	for _, r := range ready {
		if r == ch {
			return []model.Channel{ch}, nil
		}
	}
	return nil, pushTargetErr(fmt.Sprintf("渠道 %s 未就绪：凭据没配齐，或者开关是关的", ch))
}

// plainMessage 纯文本渠道的排版：标题一行，正文跟在后面。
func plainMessage(title, body string) string {
	switch {
	case title == "":
		return body
	case body == "":
		return title
	}
	return title + "\n" + body
}

// htmlMessage Telegram 的 parse_mode 是 HTML。
func htmlMessage(title, body string) string {
	if title == "" {
		return html.EscapeString(body)
	}
	return "<b>" + html.EscapeString(title) + "</b>\n" + html.EscapeString(body)
}

// markdownMessage 企业微信走 markdown，标题用小标题。
func markdownMessage(title, body string) string {
	if title == "" {
		return body
	}
	return "### " + title + "\n" + body
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
		// 光把这条通知标成「已忽略」是拦不住下一条消息的：扫描只看远端内容变没变，
		// 下一轮照样判成「有更新」并新建一条通知。必须把「用户已放行这一版」
		// 记到监控项上，消息里那句「同一个版本不会再次提醒」才是真的。
		//
		// 远端没有对应的监控项：它的去重靠通知记录本身（见 HasNodeNotificationKey），
		// 这条记录就在库里，天然不会再推第二遍。
		if n.NodeID == 0 {
			s.rememberIgnored(n)
		}
		return actionOutcome{
			Toast:  "已忽略该版本",
			State:  model.CardIgnored,
			Detail: "已忽略本版本，同一个版本不会再次提醒。\n后续出现更高的版本时仍会通知。",
		}

	case ActionSnooze:
		if err := s.st.SetNotificationCardState(n.ID, model.CardSnoozed); err != nil {
			s.log.Warn("更新通知状态失败", "err", err)
		}
		// 「稍后提醒」靠清掉「已提醒过」标记实现：不清的话下一轮扫描会认为
		// 这一版早说过了，那张卡片就成了最后一次提醒，卡片上写的
		// 「下次巡检会重新推送」变成空话。
		for _, m := range n.AllMembers() {
			img, err := s.st.FindImageByContainer(m.ContainerID)
			if err != nil || img == nil {
				img = s.findImageForNotification(&model.Notification{ImageRef: m.ImageRef, ContainerID: m.ContainerID})
			}
			if img == nil {
				continue
			}
			if err := s.st.ClearImageNotified(img.ID); err != nil {
				s.log.Warn("清除已提醒标记失败", "err", err)
			}
		}
		return actionOutcome{
			Toast:  "已设为稍后",
			State:  model.CardSnoozed,
			Detail: "已记录。本轮巡检不会重复提醒，下次巡检如有更新会重新推送。",
		}

	case ActionUpdate:
		if n.CardState == model.CardUpdating {
			// 卡片写着「更新中」，但那边其实没有任务在跑 —— 这中间断过：
			// Hub 重启、事件帧丢了，或者更早的版本压根没把状态写进库里。
			// 这种卡片最难受的地方是「再点一次」也永远只会回「进行中」，
			// 用户没有任何办法把它收尾，只能等下一条新提醒把它盖过去。
			//
			// 所以先问一句「真的还在跑吗」，不在跑就当成一次新的更新重新下发；
			// 容器若已经在目标镜像上，updater 会直接判定成功、不走重建。
			if s.updateStillRunning(n) {
				// 重复点击：不再起第二个任务，但要把卡片刷成当前状态。
				// 早先这里返回的 State 是空的，于是一连点两下就是「两下都没反应」，
				// 用户会以为第一次也没生效。
				return actionOutcome{
					Toast:  "更新已在进行中",
					State:  model.CardUpdating,
					Detail: "这条更新的任务已经在跑了，完成后这里会自动变成结果，不用重复点。",
				}
			}
			s.log.Info("卡片停在更新中但任务已不在运行，按陈旧状态重新执行",
				"notif", n.ID, "node", n.NodeID, "container", n.ContainerName)
			if err := s.st.SetNotificationCardState(n.ID, model.CardPending); err != nil {
				s.log.Warn("重置陈旧的通知状态失败", "err", err)
			}
			n.CardState = model.CardPending
		}
		// 合并提醒（一个 compose 项目里的多个容器）走批处理：
		// 依次重建，一次一个，中途失败就停在那一个上。
		if n.IsGroup() {
			return s.startBatch(n)
		}
		// 远端节点的更新要下发指令到那台机器，走的是完全另一条路。
		if n.NodeID > 0 {
			return s.applyRemoteUpdate(n)
		}
		taskID, err := s.TriggerUpdate(n, string(from))
		if err != nil {
			// 本地失败同样要把原因写到库里：卡片外观会随消息刷新丢掉，
			// 而网页上这条记录会一直留着，得让它自己说得清为什么没动。
			if serr := s.st.SetNotificationCardResult(n.ID, model.CardFailed, "更新失败："+err.Error()); serr != nil {
				s.log.Warn("更新通知状态失败", "err", serr)
			}
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
// applyRemoteUpdate 处理远端提醒上的「更新」按钮。
//
// 与本地更新的区别只有一点：不建本地任务、不碰本机 Docker，而是把指令
// 下发给那台机器。结果的回写走 NodeOpEvent —— Agent 那边跑完才会发 event 帧，
// 这里只负责把卡片从「待更新」翻成「更新中」。
func (s *Service) applyRemoteUpdate(n *model.Notification) actionOutcome {
	s.remoteMu.RLock()
	r := s.remote
	s.remoteMu.RUnlock()
	if r == nil {
		return actionOutcome{Toast: "无法更新", Detail: "远端更新能力未就绪，请重启服务后再试"}
	}
	if !r.Online(n.NodeID) {
		// 不改卡片状态：节点离线是暂时的，标成「失败」反而要用户去清。
		// 卡片保持待更新，等那台机器回来再点一次就行。
		return actionOutcome{Toast: "节点不在线，稍后再试"}
	}
	where := n.NodeName
	if where == "" {
		where = "远程节点"
	}
	// 「更新中」必须先落库，再下发指令。
	//
	// 卡片上那个「正在更新」只是这次回调的响应里换的一版外观，库里并没有跟着变。
	// 而节点跑完以后回传 done 时，Hub 是拿 card_state='updating' 去认这条通知的
	// （见 NodeOpEvent）——库里还停在 pending 就一条也找不到，卡片便永远停在
	// 「正在更新」：网页横幅早就绿了，消息里一动不动。
	//
	// 放在下发之前还有个原因：Agent 受理得很快，done 事件有可能比这个函数返回更早。
	if err := s.st.SetNotificationCardState(n.ID, model.CardUpdating); err != nil {
		s.log.Warn("更新通知状态失败", "err", err)
	}
	msg, resolved, err := r.UpdateContainer(n.NodeID, n.ContainerID, n.ContainerName)
	if err != nil {
		detail := "在 " + where + " 上执行更新失败：" + err.Error()
		// 说明一起落库。否则网页上只剩一个「失败」徽章，用户不知道该改什么，
		// 也没法把原因贴出来；卡片那边一旦被新消息盖过去，线索就彻底没了。
		if serr := s.st.SetNotificationCardResult(n.ID, model.CardFailed, detail); serr != nil {
			s.log.Warn("更新通知状态失败", "err", serr)
		}
		return actionOutcome{
			Toast:  "无法更新",
			State:  model.CardFailed,
			Detail: detail,
		}
	}
	// 下发时按名字找回了容器，说明提醒里的编号已经过期（容器被重建过）。
	// 必须把新编号写回这条通知：节点回传的结果带的是新编号，
	// 不写回就永远匹配不上，卡片会一直停在「正在更新」。
	if resolved != nil && resolved.ContainerID != "" && resolved.ContainerID != n.ContainerID {
		if serr := s.st.RealignNotificationContainer(n.ID, n.ContainerName, resolved.ContainerID); serr != nil {
			s.log.Warn("对齐通知里的容器编号失败", "notif", n.ID, "err", serr)
		}
		s.log.Info("提醒里的容器编号已过期，按容器名对齐到当前实例",
			"notif", n.ID, "container", n.ContainerName,
			"old", shortContainerID(n.ContainerID), "new", shortContainerID(resolved.ContainerID))
	}
	return actionOutcome{
		Toast:  "已开始更新",
		State:  model.CardUpdating,
		Detail: msg + "\n正在 " + where + " 上重建容器，完成后本条消息会自动更新结果。",
	}
}

// updateStillRunning 这条通知对应的更新是否真的还在跑。
//
// 本地查有没有进行中的任务，远端问一句节点当前的操作状态。
// 查不出来（比如远端能力没注入）一律按「没在跑」处理 —— 那种情况下
// 卡片本来也没有别的出路，重新下发一次至少能让它走到结果。
func (s *Service) updateStillRunning(n *model.Notification) bool {
	if n.NodeID > 0 {
		s.remoteMu.RLock()
		r := s.remote
		s.remoteMu.RUnlock()
		return r != nil && r.Busy(n.NodeID)
	}
	if n.IsGroup() {
		// 合并提醒里正在跑的可能是任何一个成员，逐个问一遍。
		for _, m := range n.AllMembers() {
			if m.ContainerID == "" {
				continue
			}
			if busy, err := s.st.HasRunningTask(m.ContainerID); err == nil && busy {
				return true
			}
		}
		return false
	}
	if n.ContainerID == "" {
		return false
	}
	busy, err := s.st.HasRunningTask(n.ContainerID)
	return err == nil && busy
}

// ApplyUpdate 在网页上对一条提醒手动执行更新。
//
// 待处理和「更新中」都能点：前者正常下发，后者先确认是不是真在跑 ——
// 还在跑就拒绝，早就结束（进程重启、断线、旧版本没把状态写进库）就重新下发一次。
// 失败与已回滚的也能点：那次失败很可能是临时的（镜像源抽了一下、目标机磁盘满），
// 修好之后最自然的动作就是原地点一次重来，而不是先把记录删掉再等下一轮巡检。
//
// 走的是与 IM 卡片按钮完全相同的 applyAction，只是那边有卡片可回写、
// 这边把结论整理成一句话返回给页面。库里状态同样会被推进到 updating，
// 所以节点跑完回传结果时，那张 IM 卡片也能跟着翻成最终状态。
func (s *Service) ApplyUpdate(n *model.Notification) (string, error) {
	switch n.CardState {
	case model.CardPending, model.CardUpdating, model.CardFailed, model.CardRollback:
	default:
		return "", errors.New("这条提醒已经处理过了，不需要再更新")
	}
	if n.CardState == model.CardUpdating && s.updateStillRunning(n) {
		return "", errors.New("这次更新还在进行中，等它跑完再看结果")
	}
	out := s.applyAction(n, ActionUpdate, "")
	switch out.State {
	case model.CardFailed:
		return "", errors.New(out.Detail)
	case "":
		// 状态没变：节点离线、重复点击之类被挡下来的情况。这类拒绝的原因
		// 只在 Toast 里（比如「节点不在线，稍后再试」），以前这里直接返回
		// 一句空字符串加 nil，页面照样弹「已下发更新」—— 用户以为动了，
		// 实际上那条提醒还停在原位，只能一头雾水地再点一次。
		if out.Toast == "" {
			return "", errors.New("这条提醒暂时无法更新")
		}
		return "", errors.New(out.Toast)
	}
	return out.Detail, nil
}

// startBatch 开始一次「整个项目依次更新」。
//
// 一次只动一个容器：底层更新器本来就是单飞的，并发重建同一台机器上的
// 多个容器既抢端口又抢容器名，出了错还分不清是谁的锅。串行慢一点，
// 但每一步都有确定的结果，失败也能准确说清停在哪一个。
func (s *Service) startBatch(n *model.Notification) actionOutcome {
	members := n.AllMembers()
	if len(members) == 0 {
		return actionOutcome{Toast: "无法更新", State: model.CardFailed,
			Detail: "这条提醒里没有可更新的容器，删掉它等下一轮巡检重新提醒即可。"}
	}
	// 已有另一批在跑：不起第二批，否则第一个成员就会撞「已有任务在执行」。
	// 这里不写库、不改状态 —— 状态没变，就不该让用户以为点了生效。
	if !s.batchMu.TryLock() {
		return actionOutcome{Toast: "已有更新在跑", State: model.CardUpdating,
			Detail: "另一批更新还没跑完，等它结束后再点；这次没有重复下发。"}
	}
	names := batchNames(members)
	if err := s.st.SetNotificationCardResult(n.ID, model.CardUpdating, "正在依次更新："+names); err != nil {
		s.log.Warn("更新通知状态失败", "err", err)
	}
	s.log.Info("开始批量更新", "notif", n.ID, "project", n.ComposeProject,
		"containers", len(members), "node", n.NodeID)
	go s.runBatch(n)

	return actionOutcome{
		Toast:  "已开始更新",
		State:  model.CardUpdating,
		Detail: fmt.Sprintf("已受理，正在依次更新 %d 个容器（%s），一次一个。\n全部结束后本条消息会显示结果。", len(members), names),
	}
}

// runBatch 依次更新一条合并提醒里的全部容器。
//
// 分两段：**先把整批镜像拉齐，再逐个重建**。
//
// 为什么不在每个成员里「拉一个建一个」：第一个成员拉到一半失败时，前面
// 已经重建的容器就是新版了，而后面还没动的仍是旧版 —— 一个 compose 项目
// 停在新旧混搭上，旧的回不去、新的互相还起不来。预拉阶段只动镜像、不碰
// 容器，失败时整批原样不动，用户换个加速源再点一次就行。
func (s *Service) runBatch(n *model.Notification) {
	defer s.batchMu.Unlock()

	// 一个项目 6 个容器，单个最多能跑 45 分钟，整体给足 3 小时。
	ctx, cancel := context.WithTimeout(s.baseCtx, 3*time.Hour)
	defer cancel()

	members := n.AllMembers()
	// 先按 depends_on 排序再动手：下游先重建会撞上上游正在重启的那几秒，
	// 表现为「更新完网页打不开 / 502」。排不出依赖时原样返回。
	members = s.sortMembersByDeps(n, members)
	total := len(members)
	if total == 0 {
		s.finishBatch(n, model.CardFailed, "这条提醒里没有可更新的容器，删掉它等下一轮巡检重新提醒即可。")
		return
	}

	// 预拉可能要几分钟，先把卡片上的话换成「拉取镜像」，
	// 否则用户看到的一直是「正在依次更新」，而容器一个都没动。
	if err := s.st.SetNotificationCardResult(n.ID, model.CardUpdating,
		fmt.Sprintf("正在拉取 %d 个容器的镜像，拉齐后开始逐个重建（这一步不会改动容器）。", total)); err != nil {
		s.log.Warn("写回通知状态失败", "notif", n.ID, "err", err)
	}

	if name, err := s.preloadBatch(ctx, n, members); err != nil {
		who := name
		if who == "" {
			who = "本机"
		}
		s.finishBatch(n, model.CardFailed, preloadFailDetail(total, who, err))
		return
	}

	done := 0
	var failed *model.NotificationMember

	for i := range members {
		m := members[i]
		if m.State == "done" {
			// 上次跑到一半断掉了，已经成功的不要重跑一遍。
			done++
			continue
		}
		if err := s.st.UpdateNotificationMemberState(n.ID, m.ContainerID, "updating", ""); err != nil {
			s.log.Warn("记录成员状态失败", "notif", n.ID, "container", m.ContainerName, "err", err)
		}
		var err error
		if n.NodeID > 0 {
			err = s.runRemoteMember(ctx, n, &m)
		} else {
			err = s.runLocalMember(ctx, &m)
		}
		if err != nil {
			m.Error = err.Error()
			_ = s.st.UpdateNotificationMemberState(n.ID, m.ContainerID, "failed", m.Error)
			failed = &m
			break
		}
		_ = s.st.UpdateNotificationMemberState(n.ID, m.ContainerID, "done", "")
		done++
		s.log.Info("合并提醒里的一个容器更新完成", "notif", n.ID,
			"container", m.ContainerName, "progress", fmt.Sprintf("%d/%d", done, total))
	}

	state := model.CardDone
	detail := fmt.Sprintf("%d 个容器已全部更新完成，原有挂载、端口与网络配置保持不变。", done)
	if failed != nil {
		state = model.CardFailed
		detail = fmt.Sprintf("已更新 %d/%d 个容器；%s 失败：%s\n其余容器保持待更新，处理完可以点「重试更新」，已成功的不会重跑。",
			done, total, failed.ContainerName, failed.Error)
	} else if done < total {
		state = model.CardFailed
		detail = fmt.Sprintf("只完成了 %d/%d 个容器就中断了，点「重试更新」可以接着来。", done, total)
	}
	s.finishBatch(n, state, detail)
}

// preloadBatch 把这条提醒里所有待更新容器的镜像先拉下来。
//
// 返回出问题的容器名与原因。全部成功才有资格动容器。
//
// 旧 Agent 不认识预拉指令，回的是「不支持的指令」。那不是故障，是版本差：
// 认出来就整体跳过预拉，退回逐个「拉+建」，不能让一次版本差把整批挡死。
func (s *Service) preloadBatch(ctx context.Context, n *model.Notification, members []model.NotificationMember) (string, error) {
	// 目标上还有别的更新在跑：预拉也要抢同一把锁，先在整体上说清楚，
	// 否则会变成「拉第一个就报已有任务在执行」，看着像是这批自己的问题。
	// 本机和远端各判一次 —— 远端那台机器上可能是网页上刚点的一批还在跑。
	if n.NodeID == 0 {
		if s.up != nil && s.up.Busy() {
			return "", &hubproto.BusyError{}
		}
	} else if s.remoteBusy(n.NodeID) {
		return "", &hubproto.BusyError{Where: n.NodeName}
	}
	for i := range members {
		m := members[i]
		if m.State == "done" {
			continue
		}
		var err error
		if n.NodeID > 0 {
			err = s.preloadRemote(ctx, n, &m)
		} else {
			err = s.preloadLocal(ctx, &m)
		}
		if err == nil {
			continue
		}
		if unsupportedAction(err) {
			s.log.Info("节点上的 Agent 不支持预拉，退回逐个更新", "notif", n.ID, "node", n.NodeID)
			return "", nil
		}
		return m.ContainerName, err
	}
	return "", nil
}

// preloadLocal 在本机拉一个成员的镜像，不重建容器。
func (s *Service) preloadLocal(ctx context.Context, m *model.NotificationMember) error {
	taskID, err := s.st.CreateTask(&model.Task{
		ImageRef:      m.ImageRef,
		ContainerID:   m.ContainerID,
		ContainerName: m.ContainerName,
		// 「更新历史」里直接显示这一列，写清楚这是批量更新前的预拉，
		// 免得用户看到一串「更新成功」以为容器已经被重建过了。
		TriggeredBy: "预拉",
	})
	if err != nil {
		return fmt.Errorf("创建拉取任务失败: %w", err)
	}
	return s.up.Pull(ctx, updater.PullRequest{
		TaskID:   taskID,
		ImageRef: s.memberTarget(m),
		Preload:  true,
	})
}

// preloadFailDetail 预拉阶段没拉到镜像时，卡片上那句话。
//
// 两种原因必须分开劝：真拉不到（403、超时）才该换加速源；要是撞上另一把锁，
// 换多少加速源都没用 —— 那批镜像其实正在往机器上写，等它跑完再点就行。
// 把两者写成同一句话，用户只会反复换源、反复失败。
func preloadFailDetail(total int, who string, err error) string {
	if hubproto.IsBusy(err) {
		return fmt.Sprintf(
			"这批更新没有开始，%d 个容器一个都没有改动：\n%s：%s\n这跟镜像源无关，等那边跑完再点「重试更新」即可。",
			total, who, err)
	}
	return fmt.Sprintf(
		"拉取镜像失败，%d 个容器一个都没有改动：\n%s：%s\n换个镜像加速源、或等源恢复后再点「重试更新」即可。",
		total, who, err)
}

// remoteBusy 那台机器上是否已经有一个任务在跑（本批之外）。
func (s *Service) remoteBusy(nodeID int64) bool {
	s.remoteMu.RLock()
	r := s.remote
	s.remoteMu.RUnlock()
	return r != nil && r.Busy(nodeID)
}

// preloadRemote 让节点拉一个成员的镜像，不重建容器。
//
// **必须等这个镜像真的拉完再返回**。Agent 一把锁，回执「开始拉取镜像」只代表
// 受理，不等于拉完了：下一个成员紧接着下发，当场被挡成「已有更新任务正在执行」，
// 而镜像其实正在往那台机器上写。用户看到的就是「明明在拉，却报错，还让我换加速源」。
func (s *Service) preloadRemote(ctx context.Context, n *model.Notification, m *model.NotificationMember) error {
	s.remoteMu.RLock()
	r := s.remote
	s.remoteMu.RUnlock()
	if r == nil {
		return errors.New("远端更新能力未就绪，请重启服务后再试")
	}
	if !r.Online(n.NodeID) {
		return errors.New("节点不在线")
	}
	startedAt := time.Now()
	if err := r.PullContainer(n.NodeID, m.ContainerID, m.ContainerName); err != nil {
		return err
	}
	return s.waitRemoteOp(ctx, n.NodeID, startedAt, "拉取 "+m.ContainerName+" 的镜像")
}

// memberTarget 推一个成员该拉到哪个引用。
//
// 与 updateTarget 同一套判断，只是数据来自成员行而不是通知主字段：
// 合并提醒里每个容器自己的最新 tag 不一样（postgres 15.18、tengine 9.4.7）。
func (s *Service) memberTarget(m *model.NotificationMember) string {
	if m.LatestTag == "" {
		return m.ImageRef
	}
	ref, err := registry.ParseReference(m.ImageRef)
	if err != nil || version.Compare(m.LatestTag, ref.Tag) <= 0 {
		return m.ImageRef
	}
	return ref.WithTag(m.LatestTag)
}

// unsupportedAction 判断节点是不是根本不认识这条指令（Agent 版本过旧）。
func unsupportedAction(err error) bool {
	return err != nil && strings.Contains(err.Error(), "不支持的指令")
}

// finishBatch 写回整批的最终结果，并把所有投递过的卡片改成同一个样子。
func (s *Service) finishBatch(n *model.Notification, state model.CardState, detail string) {
	if err := s.st.SetNotificationCardResult(n.ID, state, detail); err != nil {
		s.log.Warn("写回批量更新结果失败", "notif", n.ID, "err", err)
	}
	_ = s.st.MarkNotificationRead(n.ID, true)

	editCtx, cancelEdit := context.WithTimeout(s.baseCtx, 30*time.Second)
	defer cancelEdit()
	for _, d := range s.deliveryTargets(n) {
		s.editDelivery(editCtx, n, d, state, detail)
	}
	s.log.Info("批量更新结束", "notif", n.ID, "project", n.ComposeProject, "state", state)
}

// runLocalMember 重建本机的一个容器，阻塞到出结果。
func (s *Service) runLocalMember(ctx context.Context, m *model.NotificationMember) error {
	local := &model.Notification{
		ContainerID:   m.ContainerID,
		ContainerName: m.ContainerName,
		ImageRef:      m.ImageRef,
		LocalTag:      m.LocalTag,
		LatestTag:     m.LatestTag,
	}
	taskID, err := s.createUpdateTask(local, "batch")
	if err != nil {
		return err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- s.executeUpdate(local, taskID, "batch") }()

	select {
	case <-ctx.Done():
		return errors.New("等待更新结果超时")
	case runErr := <-errCh:
		if runErr == nil {
			return nil
		}
		// 回滚要说得更具体：用户看到的应该是「已经退回去了」，
		// 而不是一句干巴巴的健康检查失败。
		if t, terr := s.st.GetTask(taskID); terr == nil && t.Status == model.TaskRolledBack {
			return fmt.Errorf("新容器未通过健康检查，已回滚到原版本：%v", runErr)
		}
		return runErr
	}
}

// runRemoteMember 让节点重建一个容器，并等它出结果。
func (s *Service) runRemoteMember(ctx context.Context, n *model.Notification, m *model.NotificationMember) error {
	s.remoteMu.RLock()
	r := s.remote
	s.remoteMu.RUnlock()
	if r == nil {
		return errors.New("远端更新能力未就绪，请重启服务后再试")
	}
	if !r.Online(n.NodeID) {
		return errors.New("节点不在线")
	}
	startedAt := time.Now()
	_, resolved, err := r.UpdateContainer(n.NodeID, m.ContainerID, m.ContainerName)
	if err != nil {
		return err
	}
	// 编号过期（容器被重建过）时按名字找回了新编号，写回提醒与成员行，
	// 否则后面的事件对不上这条提醒。
	if resolved != nil && resolved.ContainerID != "" && resolved.ContainerID != m.ContainerID {
		if serr := s.st.RealignNotificationContainer(n.ID, m.ContainerName, resolved.ContainerID); serr != nil {
			s.log.Warn("对齐通知里的容器编号失败", "notif", n.ID, "err", serr)
		}
		m.ContainerID = resolved.ContainerID
	}
	return s.waitRemoteOp(ctx, n.NodeID, startedAt, "重建 "+m.ContainerName)
}

// waitRemoteOp 等节点上这一轮操作跑出结果。startedAt 是下发指令**之前**取的时刻。
//
// 重建和预拉都要等：Agent 一把锁，回执只代表受理，前一个还在跑就下发下一个，
// 会被当场挡回来（见 preloadRemote 的注释）。
//
// 不能用「Busy 变 false 就算完」：下发后事件要过几百毫秒才到，那会儿 Busy
// 还是 false，会被当成「已经结束」。所以先看有没有开始时间不早于下发时刻的结果，
// 再看 Busy 是否起落过。
//
// what 只进超时那句话（「重建 safeline-mgt：等待节点回传结果超时」），
// 一批六七个容器的时候，光一句「超时」根本分不清卡在哪一个。
func (s *Service) waitRemoteOp(ctx context.Context, nodeID int64, startedAt time.Time, what string) error {
	s.remoteMu.RLock()
	r := s.remote
	s.remoteMu.RUnlock()
	if r == nil {
		return errors.New("远端更新能力未就绪，请重启服务后再试")
	}

	deadline := time.Now().Add(45 * time.Minute)
	sawBusy := false
	for {
		phase, msg, updatedAt := r.OpResult(nodeID)
		// 用「不早于」而不是「晚于」：Windows 上 time.Now() 的分辨率可以粗到
		// 十几毫秒，下发与事件落在同一个刻度里时 After 是 false —— 明明跑完了，
		// 这里却一直等下去，整批更新就那么卡在「更新中」。
		if !updatedAt.IsZero() && !updatedAt.Before(startedAt) {
			switch phase {
			case hubproto.PhaseDone:
				return nil
			case hubproto.PhaseFailed:
				if msg == "" {
					msg = "节点没有说明原因，可到节点页查看 Agent 日志"
				}
				return errors.New(msg)
			}
		}
		if r.Busy(nodeID) {
			sawBusy = true
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s：等待节点回传结果超时", what)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s：等待节点回传结果超时", what)
		case <-time.After(time.Second):
		}
		// 超过一分钟还没看到任何动静，说明指令被吞了（Agent 卡住、断线）。
		if !sawBusy && time.Since(startedAt) > time.Minute {
			return fmt.Errorf("%s：节点没有回传更新进度，稍后到节点页确认状态", what)
		}
	}
}

// batchNames 合并提醒里各容器名的紧凑写法，用于卡片与日志。
func batchNames(members []model.NotificationMember) string {
	const limit = 4
	names := make([]string, 0, limit)
	for _, m := range members {
		if len(names) >= limit {
			break
		}
		if m.ContainerName != "" {
			names = append(names, m.ContainerName)
		}
	}
	out := strings.Join(names, "、")
	if len(members) > len(names) {
		out += fmt.Sprintf(" 等 %d 个", len(members))
	}
	return out
}

// NodeOpEvent 处理远端更新的进度，把最终结果写回卡片。
//
// 只认 done / failed：中间那些「拉取镜像、停止旧容器」既没有必要刷到卡片上
// （会不停闪烁），也不改变任何状态。
func (s *Service) NodeOpEvent(nodeID int64, ev hubproto.Event) {
	if ev.Phase != hubproto.PhaseDone && ev.Phase != hubproto.PhaseFailed {
		return
	}
	list, err := s.st.ListUpdatingNodeNotifications(nodeID, ev.ContainerID, ev.ContainerName)
	if err != nil {
		s.log.Warn("查询节点通知失败", "node_id", nodeID, "err", err)
		return
	}
	if len(list) == 0 {
		// 卡片可能已经被用户标成已读或忽略（那就不再改它），也可能是从网页发起的
		// 更新，本来就没有卡片。两种情况都不需要做什么。
		return
	}

	state := model.CardDone
	detail := "容器已在新镜像上重建完成，原有挂载、端口与网络配置保持不变。"
	if ev.Phase == hubproto.PhaseFailed {
		state = model.CardFailed
		detail = "更新失败：" + ev.Message
		if detail == "更新失败：" {
			detail = "更新失败，节点没有说明原因，可到节点页查看 Agent 日志。"
		}
	}

	ctx, cancel := context.WithTimeout(s.baseCtx, 30*time.Second)
	defer cancel()
	for i := range list {
		n := &list[i]
		if n.IsGroup() {
			// 合并提醒的结果由批处理统一写：这里给单个成员写一次「完成」，
			// 会把整条卡片提前标成完成，后面几个容器还在跑。
			continue
		}
		if err := s.st.SetNotificationCardResult(n.ID, state, detail); err != nil {
			s.log.Warn("写回节点通知状态失败", "id", n.ID, "err", err)
		}
		_ = s.st.MarkNotificationRead(n.ID, true)
		for _, d := range s.deliveryTargets(n) {
			s.editDelivery(ctx, n, d, state, detail)
		}
	}
	s.log.Info("节点更新结果已写回卡片", "node_id", nodeID,
		"container", ev.ContainerName, "phase", ev.Phase, "cards", len(list))
}

// rememberIgnored 把「用户放行了这一版」记到监控项上。
//
// 两种更新的记法不一样：更高版本 tag 记在 notified_tag，同 tag 内容变化记在
// ignored_digest。按 kind 分流，别混着写 —— new_tag 场景手上只有旧 tag 的
// 摘要，拿它当「已放行的内容」会把后面真正的更新一并压掉。
func (s *Service) rememberIgnored(n *model.Notification) {
	// 合并提醒里每个容器各记各的：只记主字段那一个，项目里剩下的
	// 下一轮会原样再推一遍 —— 用户刚点完「忽略」，屏幕上又冒出同一批。
	for _, m := range n.AllMembers() {
		kind := m.Kind
		if kind == "" {
			kind = n.Kind
		}
		img, err := s.st.FindImageByContainer(m.ContainerID)
		if err != nil || img == nil {
			img = s.findImageForNotification(&model.Notification{
				ImageRef: m.ImageRef, ContainerID: m.ContainerID,
			})
		}
		if img == nil {
			s.log.Warn("找不到对应的监控项，无法记录已忽略版本",
				"ref", m.ImageRef, "container", m.ContainerID)
			continue
		}
		latest := m.LatestTag
		if latest == "" {
			latest = n.LatestTag
		}
		if kind == model.NotifNewTag && latest != "" {
			if err := s.st.SetImageNotified(img.ID, latest, ""); err != nil {
				s.log.Warn("记录已忽略版本失败", "ref", m.ImageRef, "err", err)
			}
			// 摘要也照写：页面上那个「已忽略本版本」徽章与「恢复提醒」按钮
			// 都靠它判断。少了这一步，用户点完忽略回到列表里还是「有更新」，
			// 只会以为按钮没生效，再点第二次。
			if img.RemoteDigest != "" {
				if err := s.st.SetImageIgnoredDigest(img.ID, img.RemoteDigest); err != nil {
					s.log.Warn("记录已忽略摘要失败", "ref", m.ImageRef, "err", err)
				}
			}
			continue
		}
		if img.RemoteDigest == "" {
			continue
		}
		if err := s.st.SetImageIgnoredDigest(img.ID, img.RemoteDigest); err != nil {
			s.log.Warn("记录已忽略版本失败", "ref", m.ImageRef, "err", err)
		}
	}
}

// findImageForNotification 找出这条通知对应的监控项。
//
// 不能只按 (reference, container_id) 精确查：升级到更高版本 tag 之后，那一行的
// reference 已经换成新引用，而通知里存的还是发通知时的旧引用，精确查必然扑空。
// 扑空在这里的表现是「点了忽略，下一轮又提醒」，既不报错也不好查。
func (s *Service) findImageForNotification(n *model.Notification) *model.Image {
	if img, err := s.st.FindImage(n.ImageRef, n.ContainerID); err == nil {
		return img
	}
	if n.ContainerID != "" {
		if img, err := s.st.FindImageByContainer(n.ContainerID); err == nil {
			return img
		}
	}
	return nil
}

// TriggerUpdate 创建并异步执行一次更新任务，返回任务 ID。
// 三个入口（飞书按钮 / Telegram 按钮 / Web 页面）共用这一处，行为才保证一致。
func (s *Service) TriggerUpdate(n *model.Notification, triggeredBy string) (int64, error) {
	taskID, err := s.createUpdateTask(n, triggeredBy)
	if err != nil {
		return 0, err
	}
	if n.ID > 0 {
		if err := s.st.SetNotificationCardState(n.ID, model.CardUpdating); err != nil {
			s.log.Warn("更新通知状态失败", "err", err)
		}
	}

	go s.runUpdate(n, taskID, triggeredBy)
	return taskID, nil
}

// createUpdateTask 做可行性检查并落一条任务，不启动执行。
//
// 单条更新与合并提醒的批处理共用它：检查口径必须一处，否则会出现
// 「单条能更、批量报已有任务」这种一半好一半坏的状态。
func (s *Service) createUpdateTask(n *model.Notification, triggeredBy string) (int64, error) {
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
	return taskID, nil
}

// executeUpdate 真正跑一次重建，阻塞到结束。
//
// 与 runUpdate 分开是因为批处理只想拿到「成没成」，不想要「写卡片」那一段：
// 一批 6 个容器，每动一个就改一次卡片没有任何意义，结果统一在收尾时写。
func (s *Service) executeUpdate(n *model.Notification, taskID int64, triggeredBy string) error {
	ctx, cancel := context.WithTimeout(s.baseCtx, 45*time.Minute)
	defer cancel()

	target := s.updateTarget(n)
	s.log.Info("开始执行更新", "task", taskID, "container", n.ContainerName,
		"from", n.ImageRef, "target", target, "by", triggeredBy)

	return s.up.Run(ctx, updater.Request{
		TaskID:        taskID,
		ImageRef:      target,
		ContainerID:   n.ContainerID,
		ContainerName: n.ContainerName,
		TriggeredBy:   triggeredBy,
	})
}

// runUpdate 在后台执行更新，并把最终结果写回原消息。
func (s *Service) runUpdate(n *model.Notification, taskID int64, triggeredBy string) {
	err := s.executeUpdate(n, taskID, triggeredBy)

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
		if serr := s.st.SetNotificationCardResult(n.ID, state, detail); serr != nil {
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

// shortContainerID 日志里只留前十来位。完整编号 64 个字符，一行日志会被它撑满。
func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

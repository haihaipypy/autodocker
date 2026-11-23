// Package hubserver 是 Hub 侧的 WebSocket 端点。
//
// 它只负责「收」和「转发」：收下 Agent 送来的快照、按需把指令发回去。
// 判定逻辑一律留在 Agent 上，Hub 不参与 —— 否则同一套规则要在两个地方各写一遍。
package hubserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/haihaipypy/autodocker/internal/hubproto"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/store"
)

// 心跳与超时。
const (
	pingInterval = 30 * time.Second
	readTimeout  = 90 * time.Second // 三个心跳周期没任何帧就当断线
	writeTimeout = 15 * time.Second
	helloTimeout = 15 * time.Second
)

// Config Hub 侧配置。
type Config struct {
	Version string // Hub 版本，下发给 Agent 用于显示
	// ScanInterval 建议 Agent 使用的扫描间隔（秒）。0 表示不干预，
	// 沿用 Agent 自己的配置。
	ScanInterval int

	// Notifier 推送更新提醒的能力，由通知服务实现。
	//
	// 远端容器发现更新时，提醒由 **Hub** 统一发：渠道凭据配在 Hub 上，
	// Agent 那边一个都不该有。留空时只落库不推送（测试与未配渠道的场景）。
	Notifier Notifier

	// OnOp 每次收到节点的操作进度时回调，用来把「更新到哪一步了」写回卡片。
	//
	// 用回调而不是让 hubserver 直接依赖通知服务：两者本来就该互不认识，
	// 一个是 WS 端点，一个是渠道编排，硬连起来只会绕成一圈。
	OnOp func(nodeID int64, ev hubproto.Event)

	// AgentImage 镜像方式安装的节点升级时要拉的目标镜像（形如
	// haihaipypy/autodocker:1.8.4）。二进制方式不需要它 —— 那种节点直接从
	// Hub 拉同一份可执行文件。
	AgentImage string

	// AutoUpgrade 返回「是否允许自动升级节点」。
	//
	// 用回调而不是布尔值：这个开关在设置页随时可改，传值的话改完得重启才生效。
	// 为 nil 表示不自动升级，只能由用户点按钮。
	AutoUpgrade func() bool

	// Mirrors 返回要下发给节点的镜像加速地址（三态：nil 不动 / 空切片清空 / 有值替换）。
	//
	// 同样用回调：地址与开关都在设置页里，改完应当对下一次握手与下一次广播
	// 立刻生效，而不是等进程重启。为 nil 表示这个能力不存在（测试场景）。
	Mirrors func() *[]string

	// OfflineAlert 返回「要不要报警」与「失联多久算数」。
	// 为 nil 表示不报警。
	OfflineAlert func() (bool, time.Duration)

	// PushText 推一条纯文本消息，不带按钮、不进通知表。
	//
	// 节点失联没有「哪个容器要更新」这回事，硬塞进 Notification 只会让
	// 历史列表里多出一条点不动的记录。为 nil 时只写日志。
	PushText func(ctx context.Context, title, body string)
}

// Notifier 推送一条提醒，返回平台侧消息 ID。
type Notifier interface {
	Notify(ctx context.Context, n *model.Notification) (string, error)
}

// Server WebSocket 服务端。
type Server struct {
	cfg Config
	st  *store.Store
	log *slog.Logger

	upgrader websocket.Upgrader

	mu    sync.RWMutex
	conns map[int64]*conn

	// 每个节点上正在进行的操作（目前只有更新）。
	//
	// 只放内存：这是「此刻在干什么」的瞬时状态，进程重启就没有意义了 ——
	// 落库反而要处理「重启后一堆永远停在更新中」的残留。
	opsMu sync.Mutex
	ops   map[int64]*OpState

	// 升级相关的瞬时状态，同样只放内存。
	//
	// 升级会把 Agent 停掉再拉起来，这中间没有任何连接可以记录状态；
	// 唯一的持久事实是「节点回来时版本是多少」，所以这里只需要记住
	// 「刚才想把它升到哪一版」，剩下的等它回来说。
	upMu        sync.Mutex
	upgrading   map[int64]upgradeState
	lastUpgrade map[int64]hubproto.UpgradeResult
	autoTries   map[int64]autoTry

	// 批量更新（同一个 compose 项目一次点完）的状态，同样只放内存。
	// 它比 Op 多的是「一共几个、跑到第几个」，页面靠它显示进度条式的文案。
	batchMu sync.Mutex
	batches map[int64]*BatchState

	// 失联告警用。
	//
	// startedAt 是「本进程开始管事」的时刻：宽限期内不发任何失联消息。
	// 进程刚起来时所有节点都在重连，那几十秒里看到的「离线」不是故障。
	startedAt time.Time
	// seen 记本次运行里真正连上过的节点。只有它们才可能触发失联告警 ——
	// 重启后从没露面的节点，说明不了「刚掉线」，只能说明「一直没连上」。
	seenMu sync.Mutex
	seen   map[int64]bool
}

// OpState 一个节点上正在进行的操作，直接序列化给页面。
type OpState struct {
	OpID          string `json:"op_id"`
	Kind          string `json:"kind"`
	Phase         string `json:"phase"`
	ContainerID   string `json:"container_id"`
	ContainerName string `json:"container_name"`
	Message       string `json:"message"`
	TaskID        int64  `json:"task_id"` // Agent 本地库里的任务号，仅用于排查

	// HubTaskID 这次操作在 Hub 库里对应的任务号。
	//
	// 和上面那个 TaskID 是两码事：Agent 自己也建了任务，但那份记录在它本地库里，
	// Hub 读不到。让「更新历史」能显示远端任务，就得由 Hub 自己再记一份。
	HubTaskID int64     `json:"hub_task_id"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// 操作状态的保留时长。
//
// 完成态留 90 秒，是为了让页面至少有一次轮询能读到「更新完成」——
// 立刻删掉的话，用户点完按钮只会看到横幅一闪而过，不知道成没成。
// 进行中态 5 分钟没有任何更新就丢弃：Agent 崩了或者进程被 kill 时
// 不会再有 event 帧，留着只会让页面永远停在「更新中」。
const (
	opDoneTTL    = 90 * time.Second
	opRunningTTL = 5 * time.Minute
)

type conn struct {
	nodeID int64
	ws     *websocket.Conn

	writeMu sync.Mutex
	once    sync.Once
	done    chan struct{}

	// 等待回执的指令。key 是指令 ID。
	// 只有 SendWait 会用到；Send 那种「发完就不管」的指令不进这张表。
	pendingMu sync.Mutex
	pending   map[string]chan hubproto.Frame
}

// New 构造服务端。
func New(cfg Config, st *store.Store, logger *slog.Logger) *Server {
	return &Server{
		cfg:         cfg,
		st:          st,
		log:         logger,
		conns:       map[int64]*conn{},
		ops:         map[int64]*OpState{},
		upgrading:   map[int64]upgradeState{},
		lastUpgrade: map[int64]hubproto.UpgradeResult{},
		autoTries:   map[int64]autoTry{},
		batches:     map[int64]*BatchState{},
		startedAt:   time.Now(),
		seen:        map[int64]bool{},
		upgrader: websocket.Upgrader{
			HandshakeTimeout: 15 * time.Second,
			ReadBufferSize:   4096,
			WriteBufferSize:  4096,
			// Agent 不是浏览器，没有同源概念。鉴权靠 Bearer 令牌，
			// 所以这里不做 Origin 校验 —— 做了反而会把命令行场景挡掉。
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

// Online 节点当前是否连着。
func (s *Server) Online(nodeID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.conns[nodeID]
	return ok
}

// OnlineCount 在线节点数。
func (s *Server) OnlineCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.conns)
}

// Send 给某个节点下发一条指令。节点不在线时直接报错，不做排队 ——
// 排队会让用户以为点了有反应，实际要等到节点上线才执行，更容易误判。
func (s *Server) Send(nodeID int64, action string, data any) error {
	s.mu.RLock()
	c := s.conns[nodeID]
	s.mu.RUnlock()
	if c == nil {
		return errors.New("节点不在线")
	}
	id := fmt.Sprintf("c-%d-%d", nodeID, time.Now().UnixNano())
	return c.write(hubproto.Frame{Type: hubproto.TypeCmd, ID: id, Action: action}, data)
}

// SendWait 下发指令并等待 Agent 回执，返回回执里的说明文字。
//
// 和 Send 的区别就是「等不等」。更新这种破坏性操作必须等：
// 1.7.5 的 Agent 不认识 update，回执会是「不支持的指令」，页面要当场
// 告诉用户「去目标机重跑一次安装命令」；而如果只是发完就算了，
// 用户看到的会是横幅挂几分钟然后报一个看不懂的超时。
func (s *Server) SendWait(nodeID int64, action string, data any, timeout time.Duration) (string, error) {
	s.mu.RLock()
	c := s.conns[nodeID]
	s.mu.RUnlock()
	if c == nil {
		return "", errors.New("节点不在线，请稍后再试")
	}

	id := fmt.Sprintf("c-%d-%d", nodeID, time.Now().UnixNano())
	ch := make(chan hubproto.Frame, 1)
	c.addPending(id, ch)
	defer c.dropPending(id)

	if err := c.write(hubproto.Frame{Type: hubproto.TypeCmd, ID: id, Action: action}, data); err != nil {
		return "", err
	}

	select {
	case f := <-ch:
		if !f.OK {
			msg := f.Error
			if msg == "" {
				msg = "节点拒绝了这条指令"
			}
			return "", errors.New(msg)
		}
		var ack hubproto.CmdAck
		_ = json.Unmarshal(f.Data, &ack)
		return ack.Message, nil
	case <-c.done:
		return "", errors.New("节点连接已断开")
	case <-time.After(timeout):
		return "", errors.New("节点没有响应，可能是 Agent 版本过旧，请在目标机重跑一次安装命令")
	}
}

// UpdateContainer 在指定节点上重建一个容器，返回受理说明。
//
// 网页上的「更新」按钮和通知卡片上的按钮都走这里：两条入口如果各拼一次指令，
// 迟早会出现「网页能更、卡片点不动」这种一半好一半坏的状态。
//
// 指令里只带容器编号，镜像引用由 Agent 用自己那份巡检结果推导 ——
// Hub 手上的快照本来就来自 Agent，再算一遍只是多一处可能算错的地方。
//
// 编号查不到时按容器名再找一次：编号只在「这一个容器实例」的生命周期里有效，
// compose 重建、外部升级都会让它变掉，而提醒里存的是发提醒那一刻的编号。
// 只认编号的话，用户在提醒上点「重试」永远只会得到「这台机器上找不到该容器」。
//
// 第二个返回值是实际下发的那个容器快照，调用方要用它把提醒上的编号对齐 ——
// 节点跑完回传的事件带的是新编号，不写回通知，结果就永远对不上。
func (s *Server) UpdateContainer(nodeID int64, containerID, containerName string) (string, *model.NodeContainer, error) {
	if containerID == "" && containerName == "" {
		return "", nil, errors.New("缺少容器标识")
	}
	c, err := s.st.FindNodeContainer(nodeID, containerID)
	if err != nil {
		c, err = s.st.FindNodeContainerByName(nodeID, containerName)
	}
	if err != nil {
		return "", nil, errors.New("Hub 上没有这个容器的记录：可能刚被重建、快照还没更新，先到节点页刷新，等巡检回来再试")
	}
	msg, err := s.SendWait(nodeID, hubproto.ActionUpdate, hubproto.Cmd{
		ContainerID:   c.ContainerID,
		ContainerName: c.ContainerName,
		ImageRef:      c.ImageRef,
	}, 8*time.Second)
	if err != nil {
		return "", c, s.explainBusy(nodeID, err)
	}
	if msg == "" {
		msg = "已开始更新"
	}
	return msg, c, nil
}

// PullContainer 只让节点把镜像拉下来，不重建容器。
//
// 批量更新的第一步用它先把整个项目的镜像拉齐。错误可能是三类，调用方要分清：
// 节点不在线（不该继续）、找不到容器记录（快照过期）、以及「不支持的指令」
// —— 那说明节点上的 Agent 还没升到支持预拉的版本，应当退化成逐个更新。
func (s *Server) PullContainer(nodeID int64, containerID, containerName string) error {
	if containerID == "" && containerName == "" {
		return errors.New("缺少容器标识")
	}
	c, err := s.st.FindNodeContainer(nodeID, containerID)
	if err != nil {
		c, err = s.st.FindNodeContainerByName(nodeID, containerName)
	}
	if err != nil {
		return errors.New("Hub 上没有这个容器的记录：可能刚被重建、快照还没更新，先到节点页刷新，等巡检回来再试")
	}
	_, err = s.SendWait(nodeID, hubproto.ActionPull, hubproto.Cmd{
		ContainerID:   c.ContainerID,
		ContainerName: c.ContainerName,
		ImageRef:      c.ImageRef,
	}, 8*time.Second)
	return s.explainBusy(nodeID, err)
}

// explainBusy 把 Agent 那句「已有更新任务正在执行」翻成「谁在占着这台机器」。
//
// 那句原话里没有任何主体信息：用户刚点完一批更新，看到「已有任务在执行」
// 只会以为是自己这一批出了问题。实际上多半是页面上或卡片上另一条指令
// 还在拉镜像 —— 说清是谁、下一步该等什么，才不至于让人去换加速源瞎试。
func (s *Server) explainBusy(nodeID int64, err error) error {
	if !hubproto.IsBusy(err) {
		return err
	}
	who := ""
	if op := s.Op(nodeID); op != nil {
		who = op.ContainerName
	}
	return &hubproto.BusyError{Who: who}
}

// errUnsupportedAction 判断节点是不是根本不认识这条指令。
//
// 旧 Agent 的指令白名单只有 scan / update / upgrade，收到 pull 会明确回
// 「不支持的指令: pull」。这不是故障，是版本差 —— 认出来就该退化，
// 而不是把整批更新判失败，让用户以为镜像或节点坏了。
func errUnsupportedAction(err error) bool {
	return err != nil && strings.Contains(err.Error(), "不支持的指令")
}

// Op 返回节点当前正在进行的操作，没有则返回 nil。
func (s *Server) Op(nodeID int64) *OpState { return s.Ops()[nodeID] }

// OpResult 最近一次操作的阶段、说明与最后更新时间。
//
// 批量更新靠它判断「这一个成员跑完了没有、成没成」：只问 Busy 不够，
// 完成态还会留 90 秒，紧接着下发下一个成员时会误把上一次的结果算到这一次头上，
// 所以要把更新时间一起带出去，让调用方只认下发之后产生的那个结果。
func (s *Server) OpResult(nodeID int64) (phase, message string, updatedAt time.Time) {
	op := s.Op(nodeID)
	if op == nil {
		return "", "", time.Time{}
	}
	return op.Phase, op.Message, op.UpdatedAt
}

// Busy 这台机器此刻是否真的还在跑容器更新。
//
// 与 Op 分开写是因为语义不同：Op 是「最近一次操作长什么样」（完成态还会留 90 秒），
// 而这里是「现在有没有在跑」，用来判断一张停在「更新中」的卡片是不是僵尸状态。
func (s *Server) Busy(nodeID int64) bool {
	op := s.Op(nodeID)
	if op == nil {
		return false
	}
	return op.Phase != hubproto.PhaseDone && op.Phase != hubproto.PhaseFailed
}

// Ops 返回所有节点当前的操作状态，顺手清掉已过期的。
//
// 过期判定分两档：完成态留 90 秒让页面至少读到一次结果；
// 进行中态 5 分钟没有新消息就丢弃 —— Agent 崩了不会再有 event 帧，
// 留着只会让页面永远停在「更新中」。
func (s *Server) Ops() map[int64]*OpState {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	out := make(map[int64]*OpState, len(s.ops))
	for id, op := range s.ops {
		ttl := opRunningTTL
		if op.Phase == hubproto.PhaseDone || op.Phase == hubproto.PhaseFailed {
			ttl = opDoneTTL
		}
		if time.Since(op.UpdatedAt) > ttl {
			delete(s.ops, id)
			continue
		}
		cp := *op
		out[id] = &cp
	}
	return out
}

// handleEvent 收到 Agent 的操作进度。
func (s *Server) handleEvent(nodeID int64, name string, f hubproto.Frame) {
	var ev hubproto.Event
	if err := json.Unmarshal(f.Data, &ev); err != nil {
		s.log.Debug("解析进度事件失败", "node", name, "err", err)
		return
	}
	now := time.Now()

	s.opsMu.Lock()
	prev := s.ops[nodeID]
	// 换了 OpID 就是新一次操作，重新计时；同一个 OpID 只更新状态。
	if prev == nil || prev.OpID != ev.OpID {
		prev = &OpState{OpID: ev.OpID, StartedAt: now}
		s.ops[nodeID] = prev
	}
	prev.Kind = ev.Kind
	prev.Phase = ev.Phase
	prev.ContainerID = ev.ContainerID
	prev.ContainerName = ev.ContainerName
	prev.Message = ev.Message
	if ev.TaskID > 0 {
		prev.TaskID = ev.TaskID
	}
	prev.UpdatedAt = now
	hubTask := prev.HubTaskID
	s.opsMu.Unlock()

	// 历史记录先落，再谈进度：任务行不存在的话，后面那些进度日志没有地方可写。
	s.trackTask(nodeID, name, ev, hubTask)

	switch ev.Phase {
	case hubproto.PhaseDone:
		s.log.Info("节点更新完成", "node", name, "container", ev.ContainerName)
	case hubproto.PhaseFailed:
		s.log.Warn("节点更新失败", "node", name, "container", ev.ContainerName, "err", ev.Message)
	default:
		s.log.Debug("节点进度", "node", name, "phase", ev.Phase, "msg", ev.Message)
	}

	// 卡片状态回写交给通知服务。放在后面：它不改变 Hub 自己的状态，
	// 就算推送那边出问题，页面上的进度条也已经在上面更新过了。
	if s.cfg.OnOp != nil {
		s.cfg.OnOp(nodeID, ev)
	}
}

// trackTask 维护 Hub 侧的远端更新任务记录。
//
// 只在 start 时建行，progress 追加日志，结束时落状态。Hub 不重试也不判断成败，
// 结论完全照 Agent 报的那一帧写 —— 谁执行的谁负责判定。
func (s *Server) trackTask(nodeID int64, nodeName string, ev hubproto.Event, taskID int64) int64 {
	switch ev.Phase {
	case hubproto.PhaseStart:
		if taskID > 0 {
			return taskID
		}
		name := ev.ContainerName
		ref := ""
		if c, err := s.st.FindNodeContainer(nodeID, ev.ContainerID); err == nil {
			if name == "" {
				name = c.ContainerName
			}
			ref = c.ImageRef
		}
		id, err := s.st.CreateTask(&model.Task{
			ImageRef:      ref,
			ContainerID:   ev.ContainerID,
			ContainerName: name,
			NodeID:        nodeID,
			NodeName:      nodeName,
			TriggeredBy:   "hub",
		})
		if err != nil {
			s.log.Warn("创建远端任务记录失败", "node", nodeName, "err", err)
			return 0
		}
		if err := s.st.StartTask(id, ""); err != nil {
			s.log.Warn("标记远端任务开始失败", "task", id, "err", err)
		}
		s.opsMu.Lock()
		if op := s.ops[nodeID]; op != nil && op.OpID == ev.OpID {
			op.HubTaskID = id
		}
		s.opsMu.Unlock()
		return id

	case hubproto.PhaseProgress:
		if taskID > 0 && ev.Message != "" {
			line := "[" + time.Now().Format("15:04:05") + "] " + ev.Message
			if err := s.st.AppendTaskLog(taskID, line); err != nil {
				s.log.Debug("写入远端任务日志失败", "task", taskID, "err", err)
			}
		}

	case hubproto.PhaseDone:
		if taskID > 0 {
			if err := s.st.FinishTask(taskID, model.TaskSuccess, "", ""); err != nil {
				s.log.Warn("结束远端任务失败", "task", taskID, "err", err)
			}
		}

	case hubproto.PhaseFailed:
		if taskID > 0 {
			if err := s.st.FinishTask(taskID, model.TaskFailed, "", ev.Message); err != nil {
				s.log.Warn("结束远端任务失败", "task", taskID, "err", err)
			}
		}
	}
	return taskID
}

// HandleWS 处理 GET /api/agent/ws。
//
// 鉴权走 Authorization: Bearer <节点令牌>，与网页登录的 Basic 认证是两套：
// Agent 是程序，不该被逼着保管管理员口令。
func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		http.Error(w, "缺少节点令牌", http.StatusUnauthorized)
		return
	}
	node, err := s.st.FindNodeByTokenHash(hashToken(token))
	if err != nil {
		// 不区分「令牌不存在」与「令牌已吊销」，避免被拿来枚举
		http.Error(w, "节点令牌无效", http.StatusUnauthorized)
		return
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Warn("WebSocket 升级失败", "node", node.Name, "err", err)
		return
	}
	defer ws.Close()

	_ = ws.SetReadDeadline(time.Now().Add(helloTimeout))
	var hello hubproto.Frame
	if err := ws.ReadJSON(&hello); err != nil {
		s.log.Warn("节点未发送注册帧", "node", node.Name, "err", err)
		return
	}
	if hello.Type != hubproto.TypeHello {
		s.log.Warn("节点首帧不是 hello", "node", node.Name, "type", hello.Type)
		return
	}
	var h hubproto.Hello
	_ = json.Unmarshal(hello.Data, &h)
	if h.Protocol != hubproto.Protocol {
		_ = ws.WriteJSON(hubproto.Frame{
			Type:  hubproto.TypeFatal,
			Error: fmt.Sprintf("协议版本不兼容：节点 %d，Hub %d，请升级 Agent", h.Protocol, hubproto.Protocol),
		})
		s.log.Warn("节点协议版本不兼容", "node", node.Name, "node_protocol", h.Protocol, "hub_protocol", hubproto.Protocol)
		return
	}

	name := node.Name
	if name == "" {
		name = h.NodeName
		if name == "" {
			name = h.Hostname
		}
		if name != "" {
			_ = s.st.UpdateNodeMeta(node.ID, name, node.Note)
		}
	}
	if err := s.st.UpdateNodeHello(node.ID, h.Hostname, h.OS, h.Arch, h.AgentVersion, h.Protocol); err != nil {
		s.log.Warn("记录节点注册信息失败", "node", name, "err", err)
	}

	c := &conn{nodeID: node.ID, ws: ws, done: make(chan struct{}), pending: map[string]chan hubproto.Frame{}}
	s.register(c)
	defer s.unregister(c)

	// 把 Hub 侧的加速地址随回执一起带下去：Agent 拿到就能立刻用上，
	// 不用等下一次配置广播。三态语义见 HelloOK.Mirrors 的注释。
	var mirrors *[]string
	if s.cfg.Mirrors != nil {
		mirrors = s.cfg.Mirrors()
	}
	if err := c.write(hubproto.Frame{Type: hubproto.TypeHelloOK}, hubproto.HelloOK{
		NodeID:       node.ID,
		NodeName:     name,
		HubVersion:   s.cfg.Version,
		ScanInterval: s.cfg.ScanInterval,
		Mirrors:      mirrors,
	}); err != nil {
		s.log.Warn("回执发送失败", "node", name, "err", err)
		return
	}
	s.markSeen(node.ID)
	// 之前报过「失联」的节点回来了，补一条恢复消息 —— 否则那条告警
	// 会永远停在「它挂了」，用户不会再去看一眼。
	s.reportRecovered(node.ID, name)
	s.log.Info("节点已上线", "node", name, "node_id", node.ID,
		"hostname", h.Hostname, "os", h.OS, "agent", h.AgentVersion)

	// 上一次升级过来确认结果：Agent 起来后第一件事就是连进来，
	// 版本对上了就是成功，比等它跑完首轮巡检再说话要快得多。
	s.confirmUpgrade(node.ID, name, h.AgentVersion)
	// 版本落后的节点由 Hub 自动推一份升级下去。放在协程里：它要先等几秒
	// 让首轮巡检跑完，不能挡住读循环。
	go s.maybeAutoUpgrade(node.ID, name, h.AgentVersion)

	// 心跳独立成协程：读循环被 report 卡住时（网络慢、包大），
	// 心跳不能跟着停 —— 否则节点会被误判离线。
	go s.heartbeat(c)

	s.readLoop(c, name)

	if err := s.st.MarkNodeOffline(node.ID); err != nil {
		s.log.Warn("标记节点离线失败", "node", name, "err", err)
	}
	s.log.Info("节点已离线", "node", name, "node_id", node.ID)
}

func (s *Server) readLoop(c *conn, name string) {
	for {
		_ = c.ws.SetReadDeadline(time.Now().Add(readTimeout))
		var f hubproto.Frame
		if err := c.ws.ReadJSON(&f); err != nil {
			select {
			case <-c.done:
			default:
				s.log.Debug("节点连接关闭", "node", name, "err", err)
			}
			return
		}
		switch f.Type {
		case hubproto.TypePong:
			_ = s.st.TouchNode(c.nodeID)
		case hubproto.TypeReport:
			s.handleReport(c.nodeID, name, f)
		case hubproto.TypeEvent:
			s.handleEvent(c.nodeID, name, f)
		case hubproto.TypeCmdAck:
			if !f.OK {
				// 配置下发失败落在 Debug：还没升级的 Agent 不认识这条指令，
				// 回一句「不支持的指令: config」是它的正常表现，不是故障。
				// 升级上来之后自然就认了，为它刷 Warn 只会让人去查不该查的地方。
				if f.Action == hubproto.ActionConfig {
					s.log.Debug("节点未接受配置下发", "node", name, "err", f.Error)
				} else {
					s.log.Warn("节点指令执行失败", "node", name, "id", f.ID, "err", f.Error)
				}
			}
			c.deliverAck(f)
		}
	}
}

func (s *Server) handleReport(nodeID int64, name string, f hubproto.Frame) {
	var rep hubproto.Report
	if err := json.Unmarshal(f.Data, &rep); err != nil {
		s.log.Warn("解析上报内容失败", "node", name, "err", err)
		return
	}
	if err := s.st.TouchNode(nodeID); err != nil {
		s.log.Warn("刷新节点心跳失败", "node", name, "err", err)
	}

	// 升级结局随升级后的第一份上报一起回来，收下它等于把这次升级结案。
	// 放在这里而不是放在后面：下面有一条「巡检失败就保留旧快照」的提前返回，
	// 那条路同样可能带着升级结果。
	if rep.Upgrade != nil {
		s.noteUpgradeResult(nodeID, name, rep.Upgrade)
	}

	// 巡检本身失败且没有任何数据时，保留上一轮快照。
	// 否则一次 Docker 抖动就会把页面上整台机器的容器清空，像是全被删了。
	if rep.ScanError != "" && len(rep.Containers) == 0 {
		s.log.Warn("节点巡检失败，保留上一轮快照", "node", name, "err", rep.ScanError)
		return
	}

	list := make([]model.NodeContainer, 0, len(rep.Containers))
	for _, c := range rep.Containers {
		list = append(list, model.NodeContainer{
			NodeID:         nodeID,
			ContainerID:    c.ContainerID,
			ContainerName:  c.ContainerName,
			ComposeProject: c.ComposeProject,
			ComposeService: c.ComposeService,
			ComposeDeps:    c.ComposeDeps,
			ImageRef:       c.ImageRef,
			ImageID:        c.ImageID,
			LocalTag:       c.LocalTag,
			LocalDigest:    c.LocalDigest,
			RemoteDigest:   c.RemoteDigest,
			LatestTag:      c.LatestTag,
			Aliases:        c.Aliases,
			Origin:         model.ImageOrigin(c.Origin),
			Status:         model.Status(c.Status),
			Mode:           model.DetectMode(c.Mode),
			Ignored:        c.Ignored,
			LastError:      c.LastError,
			CheckedAt:      tsToTime(c.CheckedAt),
		})
	}
	if err := s.st.ReplaceNodeContainers(nodeID, list); err != nil {
		s.log.Warn("保存节点快照失败", "node", name, "err", err)
		return
	}
	if err := s.st.SetNodeReported(nodeID); err != nil {
		s.log.Warn("记录节点上报时间失败", "node", name, "err", err)
	}
	s.log.Debug("收到节点上报", "node", name, "count", len(list))

	// 快照存好之后再推提醒：通知里的容器名、镜像引用都来自这份数据，
	// 反过来的话用户可能收到一条指向「还不存在的容器」的提醒。
	s.notifyUpdates(nodeID, name, list)
}

func (s *Server) heartbeat(c *conn) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if err := c.write(hubproto.Frame{Type: hubproto.TypePing}, nil); err != nil {
				c.close()
				return
			}
		}
	}
}

func (s *Server) register(c *conn) {
	s.mu.Lock()
	old := s.conns[c.nodeID]
	s.conns[c.nodeID] = c
	s.mu.Unlock()
	// 同一节点重复连接（比如 Agent 重装后旧进程还没退）时踢掉旧的，
	// 否则两边的上报会互相覆盖，状态看起来来回跳。
	if old != nil && old != c {
		old.close()
	}
}

func (s *Server) unregister(c *conn) {
	s.mu.Lock()
	if s.conns[c.nodeID] == c {
		delete(s.conns, c.nodeID)
	}
	s.mu.Unlock()
	c.close()
}

func (c *conn) write(base hubproto.Frame, data any) error {
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		base.Data = raw
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.ws.WriteJSON(base)
}

func (c *conn) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.ws.Close()
	})
}

func (c *conn) addPending(id string, ch chan hubproto.Frame) {
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
}

func (c *conn) dropPending(id string) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

// deliverAck 把回执交给正在等待的那一方。没人等就直接丢弃 ——
// Send 下发的指令本来就不带回执需求，读循环仍需照常消费这些帧。
func (c *conn) deliverAck(f hubproto.Frame) {
	c.pendingMu.Lock()
	ch := c.pending[f.ID]
	c.pendingMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- f:
	default:
	}
}

func bearerToken(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// MatchToken 恒定时间比对，避免按响应时间猜令牌。
func MatchToken(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func tsToTime(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

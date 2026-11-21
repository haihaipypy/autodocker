// Package hubserver 是 Hub 侧的 WebSocket 端点。
//
// 它只负责「收」和「转发」：收下 Agent 送来的快照、按需把指令发回去。
// 判定逻辑一律留在 Agent 上，Hub 不参与 —— 否则同一套规则要在两个地方各写一遍。
package hubserver

import (
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
}

// Server WebSocket 服务端。
type Server struct {
	cfg Config
	st  *store.Store
	log *slog.Logger

	upgrader websocket.Upgrader

	mu    sync.RWMutex
	conns map[int64]*conn
}

type conn struct {
	nodeID int64
	ws     *websocket.Conn

	writeMu sync.Mutex
	once    sync.Once
	done    chan struct{}
}

// New 构造服务端。
func New(cfg Config, st *store.Store, logger *slog.Logger) *Server {
	return &Server{
		cfg:   cfg,
		st:    st,
		log:   logger,
		conns: map[int64]*conn{},
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

	c := &conn{nodeID: node.ID, ws: ws, done: make(chan struct{})}
	s.register(c)
	defer s.unregister(c)

	if err := c.write(hubproto.Frame{Type: hubproto.TypeHelloOK}, hubproto.HelloOK{
		NodeID:       node.ID,
		NodeName:     name,
		HubVersion:   s.cfg.Version,
		ScanInterval: s.cfg.ScanInterval,
	}); err != nil {
		s.log.Warn("回执发送失败", "node", name, "err", err)
		return
	}
	s.log.Info("节点已上线", "node", name, "node_id", node.ID,
		"hostname", h.Hostname, "os", h.OS, "agent", h.AgentVersion)

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
		case hubproto.TypeCmdAck:
			if !f.OK {
				s.log.Warn("节点指令执行失败", "node", name, "id", f.ID, "err", f.Error)
			}
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

	// 巡检本身失败且没有任何数据时，保留上一轮快照。
	// 否则一次 Docker 抖动就会把页面上整台机器的容器清空，像是全被删了。
	if rep.ScanError != "" && len(rep.Containers) == 0 {
		s.log.Warn("节点巡检失败，保留上一轮快照", "node", name, "err", rep.ScanError)
		return
	}

	list := make([]model.NodeContainer, 0, len(rep.Containers))
	for _, c := range rep.Containers {
		list = append(list, model.NodeContainer{
			NodeID:        nodeID,
			ContainerID:   c.ContainerID,
			ContainerName: c.ContainerName,
			ImageRef:      c.ImageRef,
			ImageID:       c.ImageID,
			LocalTag:      c.LocalTag,
			LocalDigest:   c.LocalDigest,
			RemoteDigest:  c.RemoteDigest,
			LatestTag:     c.LatestTag,
			Aliases:       c.Aliases,
			Origin:        model.ImageOrigin(c.Origin),
			Status:        model.Status(c.Status),
			Mode:          model.DetectMode(c.Mode),
			Ignored:       c.Ignored,
			LastError:     c.LastError,
			CheckedAt:     tsToTime(c.CheckedAt),
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

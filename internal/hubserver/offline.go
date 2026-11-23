package hubserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/haihaipypy/autodocker/internal/hubproto"
	"github.com/haihaipypy/autodocker/internal/model"
)

// 失联判定的两个窗口。
//
// startupGrace 是「本进程刚起来」的宽限期。Hub 重启的那几十秒里，所有节点
// 都还没重连上，此时看到的「离线」全是假象 —— 真按它报警，用户每次升级 Hub
// 都会收到一屏「节点失联」。
const (
	startupGrace = 3 * time.Minute
	offlineTick  = 30 * time.Second
)

func (s *Server) markSeen(nodeID int64) {
	s.seenMu.Lock()
	s.seen[nodeID] = true
	s.seenMu.Unlock()
}

func (s *Server) isSeen(nodeID int64) bool {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	return s.seen[nodeID]
}

// reportRecovered 节点重新连上时补一条「已恢复」。
//
// 只在它确实报过失联时才发：正常重启、网络抖动个几秒都没到报警线，
// 「已恢复」发出去只会变成噪声。发完立刻清记号，下一次失联重新算。
func (s *Server) reportRecovered(nodeID int64, name string) {
	at, err := s.st.NodeOfflineAlertAt(nodeID)
	if err != nil || at == 0 {
		return
	}
	if err := s.st.ClearNodeOfflineAlert(nodeID); err != nil {
		s.log.Warn("清除失联记号失败", "node", name, "err", err)
	}
	s.log.Info("节点已恢复", "node", name, "node_id", nodeID)
	if s.cfg.PushText == nil {
		return
	}
	body := fmt.Sprintf("节点 %s 已重新连上 Hub，现在的状态可以在节点页看到。", name)
	go s.cfg.PushText(context.Background(), "节点已恢复："+name, body)
}

// WatchOffline 跑失联巡检，直到 ctx 结束。
//
// 与心跳是两套机制，故意不复用：心跳负责「连接还在不在」（90 秒断线就置离线，
// 页面要立刻反映），这里负责「要不要打扰用户」。前者是显示，后者是通知，
// 混在一起就只能二选一 —— 要么页面反应迟钝，要么消息刷得停不下来。
func (s *Server) WatchOffline(ctx context.Context) {
	if s.cfg.OfflineAlert == nil {
		return
	}
	ticker := time.NewTicker(offlineTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkOffline()
		}
	}
}

// checkOffline 单次判定：谁该报、报几条。
func (s *Server) checkOffline() {
	on, wait := s.cfg.OfflineAlert()
	if !on || wait <= 0 {
		return
	}
	// 刚启动的宽限期内什么都不发：所有节点都还在重连路上。
	if time.Since(s.startedAt) < startupGrace {
		return
	}

	nodes, err := s.st.ListNodes()
	if err != nil {
		s.log.Warn("读取节点列表失败（失联巡检）", "err", err)
		return
	}

	now := time.Now()
	var victims []model.Node
	watched := 0
	for _, n := range nodes {
		if !s.isSeen(n.ID) {
			// 本次运行里从没连上过：可能是一直没装好，也可能是刚加进来。
			// 这两种都不是「刚掉线」，报警只会让用户以为出了新问题。
			continue
		}
		watched++
		if n.LastSeenAt.IsZero() {
			continue
		}
		// 判定只看「最后一次见到它的时间」，不看 status。
		// status 是连接断开那一刻改的，而连接可能因为进程卡住没及时断 ——
		// 时间戳才是唯一不会骗人的依据。
		if now.Sub(n.LastSeenAt) < wait {
			continue
		}
		if at, err := s.st.NodeOfflineAlertAt(n.ID); err == nil && at > 0 {
			continue // 这次失联已经说过了
		}
		victims = append(victims, n)
	}
	if len(victims) == 0 {
		return
	}

	// 全部节点一起失联，几乎可以肯定是 Hub 这台机器的网络或出口断了。
	// 按台数发 N 条一样的消息，只会让人一条条点开确认「哦，都一样」。
	if len(victims) == watched && watched >= 2 {
		s.alertAllOffline(victims, wait)
		return
	}
	for _, n := range victims {
		s.alertOneOffline(n, wait, now)
	}
}

func (s *Server) alertOneOffline(n model.Node, wait time.Duration, now time.Time) {
	name := nodeLabel(&n)
	body := fmt.Sprintf(
		"已经 %s 没有任何回应（最后一次联系：%s）。\n\n"+
			"常见原因：机器关机或重启、网络断开、Agent 容器被停掉。\n"+
			"它重新连上后会再发一条「已恢复」，不用一直盯着。",
		humanSpan(now.Sub(n.LastSeenAt)),
		n.LastSeenAt.Format("01-02 15:04"),
	)
	s.log.Warn("节点失联，发告警", "node", name, "node_id", n.ID)
	if s.cfg.PushText != nil {
		go s.cfg.PushText(context.Background(), "节点失联："+name, body)
	}
	s.markAlerted(n.ID, now)
}

func (s *Server) alertAllOffline(victims []model.Node, wait time.Duration) {
	names := make([]string, 0, len(victims))
	for i := range victims {
		names = append(names, nodeLabel(&victims[i]))
	}
	body := fmt.Sprintf(
		"%s 都已超过 %s 没有回应。\n\n"+
			"全部节点同时失联，通常不是它们各自的问题 —— 先看 Hub 这台机器的网卡、\n"+
			"出口网络，以及本服务有没有卡死。\n"+
			"它们重新连上后会再发一条「已恢复」。",
		strings.Join(names, "、"), humanSpan(wait),
	)
	s.log.Warn("全部节点失联，发汇总告警", "nodes", strings.Join(names, ","))
	if s.cfg.PushText != nil {
		go s.cfg.PushText(context.Background(), fmt.Sprintf("%d 个节点同时失联", len(victims)), body)
	}
	now := time.Now()
	for i := range victims {
		s.markAlerted(victims[i].ID, now)
	}
}

// markAlerted 立刻落记号，而不是等消息发出去。
//
// 发消息是异步的，中间还隔着渠道 API；不先落记号，下一轮巡检（30 秒后）
// 会把同一批节点再挑出来一遍，用户就收到两条一模一样的告警。
func (s *Server) markAlerted(nodeID int64, now time.Time) {
	if err := s.st.SetNodeOfflineAlert(nodeID, now.Unix()); err != nil {
		s.log.Warn("记录失联告警时刻失败", "node_id", nodeID, "err", err)
	}
}

func nodeLabel(n *model.Node) string {
	if n.Name != "" {
		return n.Name
	}
	if n.Hostname != "" {
		return n.Hostname
	}
	return fmt.Sprintf("节点 %d", n.ID)
}

// humanSpan 把时长写成人话，精确到分钟。
//
// 不用「12m3.4s」这种：告警里没人关心秒，反而要花一秒读懂。
func humanSpan(d time.Duration) string {
	if d < time.Minute {
		return "不到 1 分钟"
	}
	mins := int(d / time.Minute)
	if mins < 60 {
		return fmt.Sprintf("%d 分钟", mins)
	}
	hours := mins / 60
	rest := mins % 60
	if hours < 24 {
		if rest == 0 {
			return fmt.Sprintf("%d 小时", hours)
		}
		return fmt.Sprintf("%d 小时 %d 分", hours, rest)
	}
	days := hours / 24
	return fmt.Sprintf("%d 天 %d 小时", days, hours%24)
}

// BroadcastConfig 把当前配置推给所有在线节点。
//
// 设置页保存后调用一次。只推给在线的：没连上的节点会在下次握手时
// 从 HelloOK 里拿到同一份配置，不需要为它们排队。
func (s *Server) BroadcastConfig() {
	if s.cfg.Mirrors == nil {
		return
	}
	m := s.cfg.Mirrors()
	if m == nil {
		return // nil 表示「Hub 不管这件事」，别动节点上的配置
	}
	s.mu.RLock()
	conns := make([]*conn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.RUnlock()
	if len(conns) == 0 {
		return
	}

	for _, c := range conns {
		go func(c *conn) {
			f := hubproto.Frame{
				Type:   hubproto.TypeCmd,
				ID:     fmt.Sprintf("cfg-%d", time.Now().UnixNano()),
				Action: hubproto.ActionConfig,
			}
			if err := c.write(f, hubproto.Cmd{Mirrors: m}); err != nil {
				s.log.Debug("下发节点配置失败", "node_id", c.nodeID, "err", err)
			}
		}(c)
	}
	s.log.Info("已向在线节点下发加速地址", "nodes", len(conns), "mirrors", strings.Join(*m, ","))
}

package hubserver

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/haihaipypy/autodocker/internal/hubproto"
	"github.com/haihaipypy/autodocker/internal/version"
)

// 升级相关的时间参数。
const (
	// upgradeTTL 「这台机器正在升级」这条状态最多挂多久。
	//
	// 升级过程中 Agent 会重启，成功与否由它回来后的第一份上报说话；
	// 但如果 helper 在换容器的那一刻被打断，节点会一直不回来，
	// 这条状态不能永远挂着，否则按钮永远是灰的。
	upgradeTTL = 5 * time.Minute

	// agentUpgradeTimeout 等 Agent 回执的上限。
	//
	// 比 update 长得多：Agent 在回执之前要先把新二进制下载完（或把新镜像拉下来），
	// 国内网络下十几 MB 下几十秒很正常。回执晚到总比误判成「没反应」好。
	agentUpgradeTimeout = 90 * time.Second

	// autoRetryCooldown 自动升级失败后多久再试。
	autoRetryCooldown = 30 * time.Minute
	// autoMaxAttempts 同一个目标版本最多自动试几次。
	//
	// 试两次还上不去，多半是这台机器的环境有问题（磁盘满、Docker 权限、
	// 拉不动镜像），再试只会一遍遍把节点重启掉。
	autoMaxAttempts = 2
)

// upgradeState 一次进行中的升级。
type upgradeState struct {
	From    string
	Target  string
	Started time.Time
	Manual  bool
}

// autoTry 自动升级的尝试记录，用来挡住失败后的无限重试。
type autoTry struct {
	target string
	count  int
	at     time.Time
}

// Upgrade 让某个节点把自己换成 Hub 这一版。
//
// 回执等的是「准备完成」而不是「升级完成」：真正换容器的动作在连接断掉之后
// 才发生，结果由那台机器回来后的第一份上报告诉我们。所以这里的返回文案是
// 「已开始」而不是「已升级」。
func (s *Server) Upgrade(nodeID int64) (string, error) {
	node, err := s.st.GetNode(nodeID)
	if err != nil {
		return "", errors.New("节点不存在")
	}
	if !s.Online(nodeID) {
		return "", errors.New("节点不在线，等它连上再试")
	}
	if s.cfg.Version == "" || s.cfg.AgentImage == "" {
		return "", errors.New("Hub 版本信息不完整，无法下发升级")
	}
	if _, ok := s.upgradingState(nodeID); ok {
		return "", errors.New("这台节点正在升级，等它回来")
	}
	if !needUpgrade(node.AgentVersion, s.cfg.Version) {
		return "", fmt.Errorf("节点已经是 %s，没有更新的版本", node.AgentVersion)
	}

	// 先挂状态再发指令：SendWait 可能要等几十秒（下载新版本），
	// 这期间用户再点一次不该又发一条。
	s.setUpgrading(nodeID, upgradeState{
		From:    node.AgentVersion,
		Target:  s.cfg.Version,
		Started: time.Now(),
		Manual:  true,
	})

	msg, err := s.SendWait(nodeID, hubproto.ActionUpgrade, hubproto.Cmd{
		TargetVersion: s.cfg.Version,
		ImageRef:      s.cfg.AgentImage,
	}, agentUpgradeTimeout)
	if err != nil {
		s.clearUpgrading(nodeID)
		if strings.Contains(err.Error(), "不支持的指令") {
			return "", fmt.Errorf("这台节点的 Agent 还不支持自动升级（当前 %s）：在目标机重跑一次安装命令即可，从这一版起以后都能自动升级", orUnknown(node.AgentVersion))
		}
		return "", err
	}
	return msg, nil
}

// maybeAutoUpgrade 节点连上来时看一眼版本，落后就自动升。
//
// 触发点选在「连上」而不是定时轮询：那正是版本信息最新、连接最稳的时刻，
// 而且天然把「刚装好的节点」和「刚被升级过的节点」都覆盖到了。
func (s *Server) maybeAutoUpgrade(nodeID int64, name, agentVer string) {
	if s.cfg.AutoUpgrade == nil || !s.cfg.AutoUpgrade() {
		return
	}
	if !needUpgrade(agentVer, s.cfg.Version) {
		return
	}
	if _, ok := s.upgradingState(nodeID); ok {
		return
	}
	if s.autoBlocked(nodeID, s.cfg.Version) {
		return
	}

	// 刚连上时先让首轮巡检跑起来：升级会重启 Agent，巡检被打断的话
	// 页面会先空一阵子，用户容易以为出了别的问题。
	time.Sleep(8 * time.Second)
	if !s.Online(nodeID) {
		return
	}
	// 有更新任务正在跑时先等它结束 —— 升级会把那个任务连着中断。
	s.waitIdle(nodeID, 15*time.Minute)
	if _, ok := s.upgradingState(nodeID); ok {
		return
	}

	s.log.Info("节点版本落后，自动升级", "node", name,
		"agent", agentVer, "hub", s.cfg.Version)
	s.noteAutoAttempt(nodeID, s.cfg.Version)
	if _, err := s.Upgrade(nodeID); err != nil {
		s.log.Warn("自动升级没能开始", "node", name, "err", err)
		// 旧 Agent 根本不认识这条指令，重试多少次都是同一句拒绝。
		// 直接把这一版记成放弃，免得每次 Hub 重启都去踢它一次。
		if strings.Contains(err.Error(), "还不支持自动升级") {
			s.blockAuto(nodeID, s.cfg.Version)
		}
	}
}

// waitIdle 等这台机器上正在跑的更新结束（或超时）。
func (s *Server) waitIdle(nodeID int64, limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		op := s.Op(nodeID)
		if op == nil || op.Phase == hubproto.PhaseDone || op.Phase == hubproto.PhaseFailed {
			return
		}
		time.Sleep(10 * time.Second)
	}
}

// setUpgrading 记录一次进行中的升级。
func (s *Server) setUpgrading(nodeID int64, st upgradeState) {
	s.upMu.Lock()
	s.upgrading[nodeID] = st
	s.upMu.Unlock()
}

func (s *Server) clearUpgrading(nodeID int64) {
	s.upMu.Lock()
	delete(s.upgrading, nodeID)
	s.upMu.Unlock()
}

func (s *Server) upgradingState(nodeID int64) (upgradeState, bool) {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	st, ok := s.upgrading[nodeID]
	if !ok {
		return upgradeState{}, false
	}
	if time.Since(st.Started) > upgradeTTL {
		delete(s.upgrading, nodeID)
		return upgradeState{}, false
	}
	return st, true
}

// IsUpgrading 给页面用：这台机器此刻是不是正在升级。
func (s *Server) IsUpgrading(nodeID int64) bool {
	_, ok := s.upgradingState(nodeID)
	return ok
}

// UpgradingIDs 正在升级的节点编号。
func (s *Server) UpgradingIDs() []int64 {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	out := make([]int64, 0, len(s.upgrading))
	for id, st := range s.upgrading {
		if time.Since(st.Started) <= upgradeTTL {
			out = append(out, id)
		}
	}
	return out
}

// Upgradable 版本落后、且此刻在线的节点。
func (s *Server) Upgradable() []int64 {
	nodes, err := s.st.ListNodes()
	if err != nil {
		return nil
	}
	out := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		if !s.Online(n.ID) {
			continue
		}
		if needUpgrade(n.AgentVersion, s.cfg.Version) {
			out = append(out, n.ID)
		}
	}
	return out
}

// noteUpgradeResult 收下节点报回来的升级结局。
//
// 无论成败都要把「正在升级」摘掉：失败时 Agent 已经回滚并重新连上，
// 挂着那条状态只会让按钮一直是灰的。
func (s *Server) noteUpgradeResult(nodeID int64, name string, res *hubproto.UpgradeResult) {
	if res == nil {
		return
	}
	if res.OK {
		s.log.Info("节点自动升级成功", "node", name, "from", res.From, "to", res.Target)
	} else {
		s.log.Warn("节点自动升级失败，已回滚", "node", name, "err", res.Error)
	}
	s.upMu.Lock()
	delete(s.upgrading, nodeID)
	s.lastUpgrade[nodeID] = *res
	s.upMu.Unlock()
}

// LastUpgrades 每个节点上一次升级的结局，给页面显示。
func (s *Server) LastUpgrades() map[int64]hubproto.UpgradeResult {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	out := make(map[int64]hubproto.UpgradeResult, len(s.lastUpgrade))
	for k, v := range s.lastUpgrade {
		out[k] = v
	}
	return out
}

// confirmUpgrade 节点重新连上时确认一次结果。
//
// 正常路径上结果由上报里的 Upgrade 字段带来；这里兜的是另一种情况 ——
// 新 Agent 起来了但那一帧没送到（连接刚建立就抖了一下），
// 版本对上了同样可以认定升级成功。
func (s *Server) confirmUpgrade(nodeID int64, name, agentVer string) {
	st, ok := s.upgradingState(nodeID)
	if !ok {
		return
	}
	if version.Compare(agentVer, st.Target) >= 0 {
		s.log.Info("节点已升级并重新上线", "node", name, "version", agentVer)
		s.upMu.Lock()
		delete(s.upgrading, nodeID)
		s.lastUpgrade[nodeID] = hubproto.UpgradeResult{OK: true, From: st.From, Target: st.Target, At: time.Now().Unix()}
		s.upMu.Unlock()
	}
}

func (s *Server) noteAutoAttempt(nodeID int64, target string) {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	rec := s.autoTries[nodeID]
	if rec.target != target {
		rec = autoTry{target: target}
	}
	rec.count++
	rec.at = time.Now()
	s.autoTries[nodeID] = rec
}

func (s *Server) blockAuto(nodeID int64, target string) {
	s.upMu.Lock()
	s.autoTries[nodeID] = autoTry{target: target, count: autoMaxAttempts, at: time.Now()}
	s.upMu.Unlock()
}

func (s *Server) autoBlocked(nodeID int64, target string) bool {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	rec, ok := s.autoTries[nodeID]
	if !ok || rec.target != target {
		return false
	}
	if rec.count >= autoMaxAttempts {
		return true
	}
	return time.Since(rec.at) < autoRetryCooldown
}

// needUpgrade 判断 Agent 是不是比 Hub 旧。
//
// 两边都必须能解析成版本号才下结论：开发机构建的 "dev" 与正式版比大小
// 没有意义，硬比只会让每个节点在开发时被反复重启。
func needUpgrade(agentVer, hubVer string) bool {
	a, b := strings.TrimSpace(agentVer), strings.TrimSpace(hubVer)
	if a == "" || b == "" {
		return false
	}
	if !version.IsVersionTag(a) || !version.IsVersionTag(b) {
		return false
	}
	return version.Compare(a, b) < 0
}

func orUnknown(v string) string {
	if strings.TrimSpace(v) == "" {
		return "未知版本"
	}
	return v
}

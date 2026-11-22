package hubserver

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/haihaipypy/autodocker/internal/composeorder"
	"github.com/haihaipypy/autodocker/internal/hubproto"
	"github.com/haihaipypy/autodocker/internal/model"
)

// 同一个 compose 项目下的容器，一次点完之后依次重建。
//
// 为什么必须串行：Agent 侧更新器带着一把锁（up.Busy），并发下发只有第一个会开工，
// 其余的当场被拒；而且同一个项目里的容器往往共用端口、网络与数据卷，
// 同时停旧建新会把正在跑的服务真的弄断。
//
// 进度复用每台机器唯一的那份 OpState（Agent 每重建一个容器都会发 start/progress/
// done），所以这里不另造一套事件通道，只是在外面记「一共几个、跑到第几个」。
//
// 一批分两段：**先全拉，再重建**。预拉阶段只动镜像、不动容器，全部成功才进入
// 重建；任何一个拉不到，整批就地停下，容器一个都没碰过。

// BatchState 一次串行重建的公开状态，直接序列化给页面。
type BatchState struct {
	Project string `json:"project"`
	Total   int    `json:"total"`
	Done    int    `json:"done"`
	Current string `json:"current"`
	// Stage 当前阶段：「拉取镜像」或「重建容器」。
	//
	// 页面拿它改按钮上的动词。用户不知道「为什么点了半天还没开始更新」，
	// 多半就是卡在预拉——那一步不动容器，只看按钮上的数字会以为没反应。
	Stage string `json:"stage"`
	// Pulled 预拉阶段已经拉好几个。重建阶段靠 Done 计数，
	// 预拉阶段容器一个都没动，两者不能共用一个数字。
	Pulled    int       `json:"pulled"`
	Failed    string    `json:"failed"`
	Error     string    `json:"error"`
	Running   bool      `json:"running"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// 等一个成员跑完的上限。单个容器 45 分钟已经是极端（大镜像 + 慢盘），
// 而整批不再单独限时：成员数不定，按个数累加更合理。
//
// memberIdle 放到 5 分钟：健康检查的观察窗口现在按容器自己的 HEALTHCHECK
// 自适应，最长可到 10 分钟。窗口内 Agent 会每 10 秒回一行进度，
// 但老版本 Agent 不发这行心跳，按 90 秒判静默会把正在正常启动的容器判死。
const (
	memberTimeout = 45 * time.Minute
	memberIdle    = 5 * time.Minute
)

// UpdateBatch 依次重建一组容器，立刻返回。
//
// 立刻返回而不是阻塞到整批跑完：一批可能有六七个容器，全跑完是十几分钟，
// HTTP 请求挂那么久只会被浏览器、反代、飞牛壳各掐一次，用户看到的是「点了没反应」。
// 结果通过 Batches() 与容器快照回传，页面轮询自己会跟上。
func (s *Server) UpdateBatch(nodeID int64, project string, items []model.NodeContainer) (string, error) {
	if len(items) == 0 {
		return "", errors.New("这个项目下没有可更新的容器")
	}
	if op := s.Op(nodeID); op != nil && op.Phase != hubproto.PhaseDone && op.Phase != hubproto.PhaseFailed {
		who := op.ContainerName
		if who == "" {
			who = "另一个容器"
		}
		return "", fmt.Errorf("这台机器正在更新 %s，等它跑完再试", who)
	}

	s.batchMu.Lock()
	if b := s.batches[nodeID]; b != nil && b.Running {
		done := b.Done
		total := b.Total
		name := b.Project
		s.batchMu.Unlock()
		return "", fmt.Errorf("正在依次更新 %s（%d/%d），等它跑完再试", name, done, total)
	}
	s.batches[nodeID] = &BatchState{
		Project:   project,
		Total:     len(items),
		Running:   true,
		StartedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	s.batchMu.Unlock()

	go s.runBatch(nodeID, orderByDeps(items))
	return fmt.Sprintf("已开始依次更新 %d 个容器", len(items)), nil
}

// orderByDeps 按 compose 的 depends_on 把一批容器排成「上游先、下游后」。
//
// 从节点页点「更新整个项目」时，成员顺序取自快照（近似创建时间或名字序），
// 跟依赖毫无关系。下游先重建就会在上游重启的那几秒里连不上 —— tengine 报 502
// 或被健康检查判失败。排序把这一层不确定性去掉。
//
// 用下标当标识而不是容器编号：镜像维度的条目编号可以是空的，下标一定唯一。
func orderByDeps(items []model.NodeContainer) []model.NodeContainer {
	if len(items) < 2 {
		return items
	}
	nodes := make([]composeorder.Node, 0, len(items))
	for i, it := range items {
		nodes = append(nodes, composeorder.Node{
			Name:    strconv.Itoa(i),
			Service: it.ComposeService,
			Deps:    it.ComposeDeps,
		})
	}
	sorted := composeorder.Sort(nodes)
	out := make([]model.NodeContainer, 0, len(items))
	for _, nd := range sorted {
		i, err := strconv.Atoi(nd.Name)
		if err != nil || i < 0 || i >= len(items) {
			continue
		}
		out = append(out, items[i])
	}
	if len(out) != len(items) {
		return items
	}
	return out
}

// BatchBusy 这台机器上有没有在跑批量更新。
//
// 单独判一次是因为单个容器的入口只看 Op，而批量刚点下去、第一个事件还没回来的
// 那几秒里 Op 是空的 —— 不挡的话，用户手快就能插进去一个单容器更新。
func (s *Server) BatchBusy(nodeID int64) bool {
	s.batchMu.Lock()
	defer s.batchMu.Unlock()
	b := s.batches[nodeID]
	return b != nil && b.Running
}

// Batches 所有节点的批量状态，供节点列表接口一起带出去。
func (s *Server) Batches() map[int64]*BatchState {
	s.batchMu.Lock()
	defer s.batchMu.Unlock()
	out := make(map[int64]*BatchState, len(s.batches))
	for id, b := range s.batches {
		cp := *b
		out[id] = &cp
	}
	return out
}

func (s *Server) runBatch(nodeID int64, items []model.NodeContainer) {
	defer func() {
		s.batchPatch(nodeID, func(b *BatchState) {
			b.Running = false
			b.Current = ""
			b.Stage = ""
		})
	}()

	// 阶段一：先把所有镜像拉到位，一个拉不到就整批不动。
	//
	// 这一段的代价是「慢一点」，换来的是「失败时什么都没变」：整个项目的
	// 容器要么全在旧版，要么全在新版。先前逐个「拉+建」，第一个成员拉到
	// 一半失败，前面几个已经换成新版了 —— 一个 compose 项目停在新旧混搭
	// 上：旧的回不去，新的互相还起不来。那才是真的难收拾。
	//
	// 旧 Agent 不认这条指令，认出来就整体跳过预拉，退化成 1.10.3 的行为。
	for _, it := range items {
		s.batchPatch(nodeID, func(b *BatchState) {
			b.Current = it.ContainerName
			b.Stage = "拉取镜像"
		})
		started := time.Now()
		if err := s.PullContainer(nodeID, it.ContainerID, it.ContainerName); err != nil {
			if errUnsupportedAction(err) {
				s.log.Info("节点上的 Agent 不支持预拉，退回逐个更新", "node", nodeID)
				break
			}
			s.failMember(nodeID, it.ContainerName, preloadFailText(err))
			return
		}
		if err := s.waitMember(nodeID, it.ContainerName, started); err != nil {
			s.failMember(nodeID, it.ContainerName, preloadFailText(err))
			return
		}
		s.batchPatch(nodeID, func(b *BatchState) { b.Pulled++ })
	}

	// 阶段二：镜像都在本地了，逐个重建。这里失败仍然即停 ——
	// 已经重建过的没法假装没动过，继续往下跑只会让混搭更乱。
	for _, it := range items {
		s.batchPatch(nodeID, func(b *BatchState) {
			b.Current = it.ContainerName
			b.Stage = "重建容器"
		})

		// 起点必须取在下发**之前**。Agent 是先回执再跑，而「开始更新」那条事件
		// 紧跟回执发出，落后几毫秒 —— 以下发之后取的时间当起点，事件就已经
		// 「早于本次开始」了，会被当成上一轮的残留，于是一直等不到结果。
		started := time.Now()
		// 走和单个更新完全相同的入口：指令怎么拼只有这一处，卡片按钮、
		// 网页单容器按钮、批量都从这儿过，不会出现三条路三种行为。
		if _, _, err := s.UpdateContainer(nodeID, it.ContainerID, it.ContainerName); err != nil {
			s.failMember(nodeID, it.ContainerName, err.Error())
			return
		}
		if err := s.waitMember(nodeID, it.ContainerName, started); err != nil {
			s.failMember(nodeID, it.ContainerName, err.Error())
			return
		}
		s.batchPatch(nodeID, func(b *BatchState) {
			b.Done++
			b.Current = ""
		})
		// 给 Agent 那次「更新后上报」留一点时间落到 Hub 再下发下一个：
		// 不给这个空档，页面上先看到的永远是上一个容器的旧状态。
		time.Sleep(600 * time.Millisecond)
	}
}

// waitMember 等一个成员跑出结果。started 是下发这条指令之前取的时刻。
//
// 只认「下发之后」产生的结果：完成态会留 90 秒，紧接着等下一个成员时
// 上次那份 done 还挂在那儿，拿它当结果会把没跑的当成跑完了。
// updatedAt 也必须是 !Before，不能写 After —— Windows 的时间精度粗到十几毫秒，
// 下发与事件同刻度时 After 为 false，整批就永远卡在那里等。
func (s *Server) waitMember(nodeID int64, name string, started time.Time) error {
	deadline := started.Add(memberTimeout)
	idle := started
	for {
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s 等了 %d 分钟还没跑完，已放弃等待", name, int(memberTimeout.Minutes()))
		}
		phase, msg, updatedAt := s.OpResult(nodeID)
		if !updatedAt.IsZero() && !updatedAt.Before(started) {
			switch phase {
			case hubproto.PhaseDone:
				return nil
			case hubproto.PhaseFailed:
				if msg == "" {
					msg = "更新失败"
				}
				return errors.New(msg)
			}
			idle = time.Now()
		}
		if time.Since(idle) > memberIdle {
			return fmt.Errorf("%s 更新中，节点 %d 秒没有回传进度", name, int(memberIdle.Seconds()))
		}
		time.Sleep(time.Second)
	}
}

// preloadFailText 预拉阶段没成功时写进批次状态的那句话。
//
// 撞上另一把锁时不能说「拉取镜像失败」：镜像根本没去拉，那句话会把人
// 引到加速源上去折腾。这时候事实就是「一条指令都没落地，整批没动」。
func preloadFailText(err error) string {
	if hubproto.IsBusy(err) {
		return err.Error() + "（整批没有改动）"
	}
	return "拉取镜像失败：" + err.Error() + "（整批没有改动）"
}

func (s *Server) failMember(nodeID int64, name, msg string) {
	s.batchPatch(nodeID, func(b *BatchState) {
		b.Failed = name
		b.Error = msg
		b.Running = false
		b.Current = ""
		b.Stage = ""
	})
}

func (s *Server) batchPatch(nodeID int64, fn func(*BatchState)) {
	s.batchMu.Lock()
	defer s.batchMu.Unlock()
	b := s.batches[nodeID]
	if b == nil {
		return
	}
	fn(b)
	b.UpdatedAt = time.Now()
}

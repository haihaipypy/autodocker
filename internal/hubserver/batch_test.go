package hubserver

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/haihaipypy/autodocker/internal/hubproto"
	"github.com/haihaipypy/autodocker/internal/model"
)

// fakeUpdater 模拟目标机上的 Agent：收到 pull / update 就回执，紧接着发一条事件。
//
// 回执与事件之间的间隔刻意压到 0：真实 Agent 也是「先回执、立刻开工」，
// 这正是批次最容易踩空的地方 —— 事件可能比 Hub 那边记录的起点还早。
type fakeUpdater struct {
	mu     sync.Mutex
	order  []string // update 的下发顺序
	pulls  []string // pull 的下发顺序
	fail   map[string]string
	pullOK map[string]string // 预拉失败的容器与原因
	noPull bool              // 模拟还没升级的老 Agent：不认识 pull
	// busyPull 模拟 Agent 上已经有一条指令占着锁：pull 当场被拒。
	busyPull bool
}

func (f *fakeUpdater) serve(c *websocket.Conn) {
	for {
		_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
		var frame hubproto.Frame
		if err := c.ReadJSON(&frame); err != nil {
			return
		}
		if frame.Type != hubproto.TypeCmd {
			continue
		}
		var cmd hubproto.Cmd
		_ = json.Unmarshal(frame.Data, &cmd)

		phase, msg := hubproto.PhaseDone, "完成"
		switch frame.Action {
		case hubproto.ActionPull:
			if f.noPull {
				raw, _ := json.Marshal(hubproto.CmdAck{})
				_ = c.WriteJSON(hubproto.Frame{Type: hubproto.TypeCmdAck, ID: frame.ID, OK: false, Error: "不支持的指令: pull", Data: raw})
				continue
			}
			if f.busyPull {
				raw, _ := json.Marshal(hubproto.CmdAck{})
				_ = c.WriteJSON(hubproto.Frame{Type: hubproto.TypeCmdAck, ID: frame.ID, OK: false, Error: hubproto.ErrBusy, Data: raw})
				continue
			}
			msg = "镜像已就绪"
			if m, ok := f.pullOK[cmd.ContainerName]; ok {
				phase, msg = hubproto.PhaseFailed, m
			}
			f.mu.Lock()
			f.pulls = append(f.pulls, cmd.ContainerName)
			f.mu.Unlock()
		case hubproto.ActionUpdate:
			msg = "更新完成"
			if m, ok := f.fail[cmd.ContainerName]; ok {
				phase, msg = hubproto.PhaseFailed, m
			}
			f.mu.Lock()
			f.order = append(f.order, cmd.ContainerName)
			f.mu.Unlock()
		default:
			raw, _ := json.Marshal(hubproto.CmdAck{})
			_ = c.WriteJSON(hubproto.Frame{Type: hubproto.TypeCmdAck, ID: frame.ID, OK: false, Error: "不支持的指令: " + frame.Action, Data: raw})
			continue
		}

		raw, _ := json.Marshal(hubproto.CmdAck{Message: "已开始"})
		_ = c.WriteJSON(hubproto.Frame{Type: hubproto.TypeCmdAck, ID: frame.ID, OK: true, Data: raw})

		ev, _ := json.Marshal(hubproto.Event{
			OpID:          frame.Action + "-" + cmd.ContainerID,
			Kind:          frame.Action,
			Phase:         phase,
			ContainerID:   cmd.ContainerID,
			ContainerName: cmd.ContainerName,
			Message:       msg,
			At:            hubproto.Now(),
		})
		_ = c.WriteJSON(hubproto.Frame{Type: hubproto.TypeEvent, Data: ev})
	}
}

func (f *fakeUpdater) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.order))
	copy(out, f.order)
	return out
}

func (f *fakeUpdater) pulled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.pulls))
	copy(out, f.pulls)
	return out
}

func batchItems(nodeID int64, names ...string) []model.NodeContainer {
	out := make([]model.NodeContainer, 0, len(names))
	for i, name := range names {
		out = append(out, model.NodeContainer{
			NodeID:         nodeID,
			ContainerID:    "c" + string(rune('1'+i)),
			ContainerName:  name,
			ImageRef:       "chaitin/" + name + ":9.4.3",
			Status:         model.StatusUpdatePending,
			Origin:         model.OriginContainer,
			ComposeProject: "safeline",
		})
	}
	return out
}

func waitBatchDone(t *testing.T, srv *Server, nodeID int64) *BatchState {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if b := srv.Batches()[nodeID]; b != nil && !b.Running {
			return b
		}
		time.Sleep(20 * time.Millisecond)
	}
	b := srv.Batches()[nodeID]
	t.Fatalf("批量更新没有结束：%+v", b)
	return nil
}

// 一批里的容器必须按顺序一个个来：Agent 一把锁，并发下发只有第一个能开工，
// 而且同一项目里的容器常常共用端口，同时重建会把服务真的弄断。
func TestUpdateBatchRunsMembersInOrder(t *testing.T) {
	srv, st, ts, nodeID := newTestHub(t)

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	sendHello(t, c, hubproto.Protocol)

	items := batchItems(nodeID, "chaos", "tengine", "luigi")
	if err := st.ReplaceNodeContainers(nodeID, items); err != nil {
		t.Fatal(err)
	}

	agent := &fakeUpdater{}
	go agent.serve(c)

	if _, err := srv.UpdateBatch(nodeID, "safeline", items); err != nil {
		t.Fatalf("批量更新没起来: %v", err)
	}
	b := waitBatchDone(t, srv, nodeID)

	// 先把三个镜像全部拉完，再开始重建 —— 顺序也必须和列表一致。
	if got, want := agent.pulled(), []string{"chaos", "tengine", "luigi"}; !equalStrings(got, want) {
		t.Fatalf("预拉顺序不对：期望 %v，实际 %v", want, got)
	}
	got := agent.seen()
	want := []string{"chaos", "tengine", "luigi"}
	if len(got) != len(want) {
		t.Fatalf("下发的容器数不对：期望 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序不对：期望 %v，实际 %v", want, got)
		}
	}
	if b.Done != 3 || b.Failed != "" {
		t.Errorf("三个都成功时应当是 3/3 无失败，实际 done=%d failed=%q err=%q", b.Done, b.Failed, b.Error)
	}
	if b.Total != 3 {
		t.Errorf("Total 应为 3，实际 %d", b.Total)
	}
}

// 失败即停：后面的容器一个都不该被下发，状态里要写清停在哪一个。
func TestUpdateBatchStopsOnFirstFailure(t *testing.T) {
	srv, st, ts, nodeID := newTestHub(t)

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	sendHello(t, c, hubproto.Protocol)

	items := batchItems(nodeID, "chaos", "tengine", "luigi")
	if err := st.ReplaceNodeContainers(nodeID, items); err != nil {
		t.Fatal(err)
	}

	agent := &fakeUpdater{fail: map[string]string{"tengine": "拉取镜像失败: no such image"}}
	go agent.serve(c)

	if _, err := srv.UpdateBatch(nodeID, "safeline", items); err != nil {
		t.Fatalf("批量更新没起来: %v", err)
	}
	b := waitBatchDone(t, srv, nodeID)

	if got := agent.seen(); len(got) != 2 {
		t.Fatalf("第二个就失败了，第三个不该被下发，实际下发 %v", got)
	}
	if b.Done != 1 {
		t.Errorf("成功的应当只有 1 个，实际 %d", b.Done)
	}
	if b.Failed != "tengine" {
		t.Errorf("失败容器应为 tengine，实际 %q", b.Failed)
	}
	if b.Error == "" || b.Error == "更新失败" {
		t.Errorf("要带上 Agent 给的真实原因，实际 %q", b.Error)
	}
}

// 预拉阶段失败时，容器一个都不能动。
//
// 这是「先全拉再重建」的全部意义：整个项目的镜像要么全到位，要么全都别动。
// 先前逐个「拉+建」，tengine 拉不到的时候 chaos 已经是新版了 —— 项目停在新旧
// 混搭上，旧的回不去、新的互相起不来。
func TestUpdateBatchPreloadFailureTouchesNothing(t *testing.T) {
	srv, st, ts, nodeID := newTestHub(t)

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	sendHello(t, c, hubproto.Protocol)

	items := batchItems(nodeID, "chaos", "tengine", "luigi")
	if err := st.ReplaceNodeContainers(nodeID, items); err != nil {
		t.Fatal(err)
	}

	agent := &fakeUpdater{pullOK: map[string]string{"tengine": "镜像源拒绝访问（HTTP 403）"}}
	go agent.serve(c)

	if _, err := srv.UpdateBatch(nodeID, "safeline", items); err != nil {
		t.Fatalf("批量更新没起来: %v", err)
	}
	b := waitBatchDone(t, srv, nodeID)

	if got := agent.seen(); len(got) != 0 {
		t.Fatalf("预拉失败时一个容器都不该被重建，实际下发了 %v", got)
	}
	if b.Done != 0 {
		t.Errorf("一个都没更新，Done 应为 0，实际 %d", b.Done)
	}
	if b.Failed != "tengine" {
		t.Errorf("失败容器应为 tengine，实际 %q", b.Failed)
	}
	if !strings.Contains(b.Error, "拉取镜像失败") || !strings.Contains(b.Error, "整批没有改动") {
		t.Errorf("要说清是预拉失败、整批没动，实际 %q", b.Error)
	}
}

// 老 Agent 不认识 pull：退化成逐个更新，不能把整批判失败。
//
// 版本差不是故障。节点会自动跟随升级，但升级前那段时间里，
// 一次「点了就报不支持」足以让人以为工具坏了。
func TestUpdateBatchFallsBackWhenPullUnsupported(t *testing.T) {
	srv, st, ts, nodeID := newTestHub(t)

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	sendHello(t, c, hubproto.Protocol)

	items := batchItems(nodeID, "chaos", "tengine")
	if err := st.ReplaceNodeContainers(nodeID, items); err != nil {
		t.Fatal(err)
	}

	agent := &fakeUpdater{noPull: true}
	go agent.serve(c)

	if _, err := srv.UpdateBatch(nodeID, "safeline", items); err != nil {
		t.Fatalf("批量更新没起来: %v", err)
	}
	b := waitBatchDone(t, srv, nodeID)

	if got, want := agent.seen(), []string{"chaos", "tengine"}; !equalStrings(got, want) {
		t.Fatalf("应当在预拉不被支持时照旧逐个更新，期望 %v，实际 %v", want, got)
	}
	if b.Done != 2 || b.Failed != "" {
		t.Errorf("两个都该成功，实际 done=%d failed=%q err=%q", b.Done, b.Failed, b.Error)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 一批没跑完时再点，必须被挡住 —— 尤其批量刚点下去、第一个事件还没回来
// 那几秒，Op 是空的，只看 Op 挡不住。
func TestUpdateBatchRejectsSecondRun(t *testing.T) {
	srv, st, ts, nodeID := newTestHub(t)
	_ = ts

	items := batchItems(nodeID, "chaos", "tengine")
	if err := st.ReplaceNodeContainers(nodeID, items); err != nil {
		t.Fatal(err)
	}

	srv.batchMu.Lock()
	srv.batches[nodeID] = &BatchState{Project: "safeline", Total: 2, Done: 1, Running: true, StartedAt: time.Now(), UpdatedAt: time.Now()}
	srv.batchMu.Unlock()

	if !srv.BatchBusy(nodeID) {
		t.Error("运行中的批次应当被认出来")
	}
	if _, err := srv.UpdateBatch(nodeID, "safeline", items); err == nil {
		t.Fatal("上一批没跑完，不该再开一批")
	}

	// 结束之后要能再点（失败重试靠的就是这个）
	srv.batchMu.Lock()
	srv.batches[nodeID].Running = false
	srv.batchMu.Unlock()
	if srv.BatchBusy(nodeID) {
		t.Error("批次结束后不该再算忙")
	}
}

// 撞上 Agent 那把锁时，别把原因说成「拉取镜像失败」。
//
// 镜像根本没去拉，是这台机器上另一条指令还在跑。写成「拉取镜像失败」，
// 用户会去换加速源，换完再点还是同一条错 —— 原因压根不在源上。
func TestUpdateBatchExplainsBusyLock(t *testing.T) {
	srv, st, ts, nodeID := newTestHub(t)

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	sendHello(t, c, hubproto.Protocol)

	items := batchItems(nodeID, "chaos", "tengine")
	if err := st.ReplaceNodeContainers(nodeID, items); err != nil {
		t.Fatal(err)
	}

	go (&fakeUpdater{busyPull: true}).serve(c)

	if _, err := srv.UpdateBatch(nodeID, "safeline", items); err != nil {
		t.Fatalf("批量更新没起来: %v", err)
	}
	b := waitBatchDone(t, srv, nodeID)

	if b.Failed != "chaos" {
		t.Errorf("卡在第一个成员上，Failed 应为 chaos，实际 %q", b.Failed)
	}
	if !strings.Contains(b.Error, "还有一个更新没跑完") || !strings.Contains(b.Error, "整批没有改动") {
		t.Errorf("要说清是撞上了另一条指令、整批没动，实际 %q", b.Error)
	}
	if strings.Contains(b.Error, "拉取镜像失败") || strings.Contains(b.Error, "稍后重试") {
		t.Errorf("这不是拉取失败，不该把人引到镜像源上：%q", b.Error)
	}
}

// 批量重建必须按 compose 的 depends_on 排顺序：上游先换，下游后换。
//
// 快照里的顺序取的是容器创建时间/名字，跟依赖没关系 —— safeline-tengine
// 很容易排在第一个。它起来时 mgt 正在重建，连不上就 502、退出、被判失败。
func TestUpdateBatchFollowsDependsOn(t *testing.T) {
	srv, st, ts, nodeID := newTestHub(t)

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	sendHello(t, c, hubproto.Protocol)

	// 故意把下游排在最前，模拟「按名字序」的快照。
	items := batchItems(nodeID, "tengine", "mgt", "pg")
	items[0].ComposeService, items[0].ComposeDeps = "tengine", []string{"mgt"}
	items[1].ComposeService, items[1].ComposeDeps = "mgt", []string{"pg"}
	items[2].ComposeService = "pg"
	if err := st.ReplaceNodeContainers(nodeID, items); err != nil {
		t.Fatal(err)
	}

	agent := &fakeUpdater{}
	go agent.serve(c)

	if _, err := srv.UpdateBatch(nodeID, "safeline", items); err != nil {
		t.Fatalf("批量更新没起来: %v", err)
	}
	waitBatchDone(t, srv, nodeID)

	// 预拉与重建都必须按依赖顺序来：先拉齐上游，再拉下游。
	want := []string{"pg", "mgt", "tengine"}
	if got := agent.pulled(); !equalStrings(got, want) {
		t.Errorf("预拉顺序没按依赖排：期望 %v，实际 %v", want, got)
	}
	if got := agent.seen(); !equalStrings(got, want) {
		t.Errorf("重建顺序没按依赖排：期望 %v，实际 %v", want, got)
	}
}

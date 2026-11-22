package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/haihaipypy/autodocker/internal/hubproto"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/store"
)

// fakeRemote 记录「往哪台机器发了什么指令」。
type fakeRemote struct {
	online bool
	err    error
	calls  []string
	msg    string

	// busy 让假节点模拟「更新还在跑」；默认 false 等于任务早就结束了。
	busy bool

	// resolved 模拟「按名字找回了另一个编号」：容器被重建过，
	// 提醒里存的是旧编号。
	resolved *model.NodeContainer

	// 操作结果，批量更新用它判断每个成员成没成。
	phase     string
	message   string
	updatedAt time.Time

	// 每下发一个成员就立刻给出结果，模拟跑得很快的节点。
	doneOnDispatch bool
	// 下发即失败，用来验证「一个失败就停下」。
	failOnDispatch string

	// 预拉阶段：记录拉过哪些容器，以及要不要让某一个失败。
	//
	// 预拉与更新分开记 —— calls 表的是「重建了哪几个」，预拉不该混进去，
	// 否则「一个容器都没动」这类断言会被预拉的记录污染。
	preloadCalls []string
	preloadErr   error
	// pullUnsupported 模拟还没升级的老 Agent：不认识 pull 指令。
	pullUnsupported bool

	// 拉取不是瞬时的：真机上一个镜像要拉几十秒到几分钟，而 Agent 一把锁，
	// 这期间再发一条 pull 会被当场挡回来 —— 老代码就是这么把整批判失败的。
	mu      sync.Mutex
	pulling bool
	// pullFor 每个镜像要拉多久；默认 0 表示「瞬间到位」，老用例靠它保持快。
	pullFor time.Duration
}

func (f *fakeRemote) Online(int64) bool { return f.online }

func (f *fakeRemote) Busy(int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	// 正在拉镜像也算忙：节点上那把锁是重建和拉取共用的。
	return f.busy || f.pulling
}

func (f *fakeRemote) OpResult(int64) (string, string, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.phase, f.message, f.updatedAt
}

func (f *fakeRemote) UpdateContainer(nodeID int64, containerID, containerName string) (string, *model.NodeContainer, error) {
	f.mu.Lock()
	f.calls = append(f.calls, containerID)
	switch {
	case f.failOnDispatch != "":
		f.phase, f.message, f.updatedAt = hubproto.PhaseFailed, f.failOnDispatch, time.Now()
	case f.doneOnDispatch:
		f.phase, f.message, f.updatedAt = hubproto.PhaseDone, "更新完成", time.Now()
	}
	f.mu.Unlock()
	if f.err != nil {
		return "", nil, f.err
	}
	// 默认把「实际下发的容器」原样回给调用方；需要模拟编号过期的用例
	// 自己塞 resolved。
	resolved := f.resolved
	if resolved == nil {
		resolved = &model.NodeContainer{ContainerID: containerID, ContainerName: containerName}
	}
	if f.msg != "" {
		return f.msg, resolved, nil
	}
	return "已开始更新", resolved, nil
}

// PullContainer 模拟真 Agent 的预拉：回执立刻给，镜像要拉一会儿才到位。
//
// 「拉取还在跑时再来一条 pull 会被挡」是这里最要紧的一条：真 Agent 一把锁，
// 老代码不等前一个拉完就发下一个，第二个成员就被这句话挡回来 ——
// 镜像明明正在往机器上写，卡片却说这一批失败、一个都没动。
func (f *fakeRemote) PullContainer(nodeID int64, containerID, containerName string) error {
	if f.pullUnsupported {
		return errors.New("不支持的指令: pull")
	}
	f.mu.Lock()
	if f.pulling {
		f.mu.Unlock()
		return &hubproto.BusyError{}
	}
	if f.preloadErr != nil {
		err := f.preloadErr
		f.mu.Unlock()
		return err
	}
	f.preloadCalls = append(f.preloadCalls, containerID)
	delay := f.pullFor
	if delay <= 0 {
		// 瞬间到位：老用例不必为了等一个假镜像多花一秒。
		f.phase, f.message, f.updatedAt = hubproto.PhaseDone, "镜像已就绪", time.Now()
		f.mu.Unlock()
		return nil
	}
	f.pulling = true
	f.phase, f.message, f.updatedAt = hubproto.PhaseProgress, "开始拉取镜像", time.Now()
	f.mu.Unlock()

	go func() {
		time.Sleep(delay)
		f.mu.Lock()
		f.pulling = false
		f.phase, f.message, f.updatedAt = hubproto.PhaseDone, "镜像已就绪", time.Now()
		f.mu.Unlock()
	}()
	return nil
}

func newRemoteService(t *testing.T, r RemoteNodes) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := &Service{st: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)), baseCtx: context.Background()}
	if r != nil {
		s.SetRemoteNodes(r)
	}
	return s, st
}

func createRemoteNotification(t *testing.T, st *store.Store, nodeID int64) *model.Notification {
	t.Helper()
	n := &model.Notification{
		ImageRef: "nginx:latest", ContainerID: "c1", ContainerName: "web",
		NodeID: nodeID, NodeName: "1panel", VersionKey: "digest:sha256:bbb",
		Kind: model.NotifNewVersion, Title: "1panel · web 有新版本可用",
		CardState: model.CardUpdating,
	}
	id, err := st.CreateNotification(n)
	if err != nil {
		t.Fatalf("写入通知失败: %v", err)
	}
	n.ID = id
	return n
}

// 节点离线时不改卡片状态：那只是暂时连不上，标成失败反而要用户去清。
func TestRemoteUpdateRejectsOfflineNode(t *testing.T) {
	r := &fakeRemote{online: false}
	s, st := newRemoteService(t, r)
	n := createRemoteNotification(t, st, 7)
	_ = st.SetNotificationCardState(n.ID, model.CardPending)

	out := s.applyRemoteUpdate(n)
	if out.State != "" {
		t.Errorf("离线时不该改卡片状态，实际 %q", out.State)
	}
	if !strings.Contains(out.Toast, "不在线") {
		t.Errorf("toast 应说明节点不在线，实际 %q", out.Toast)
	}
	if len(r.calls) != 0 {
		t.Errorf("离线时不该下发指令，实际发了 %v", r.calls)
	}
}

// 指令没被受理（比如 Agent 版本过旧）时，卡片要当场标失败并带上原因。
func TestRemoteUpdateMarksFailedOnReject(t *testing.T) {
	r := &fakeRemote{online: true, err: errors.New("不支持的指令: update")}
	s, st := newRemoteService(t, r)
	n := createRemoteNotification(t, st, 7)

	out := s.applyRemoteUpdate(n)
	if out.State != model.CardFailed {
		t.Errorf("被拒时卡片应标失败，实际 %q", out.State)
	}
	if !strings.Contains(out.Detail, "1panel") {
		t.Errorf("失败说明里要写清是哪台机器：%q", out.Detail)
	}
	if !strings.Contains(out.Detail, "不支持的指令") {
		t.Errorf("失败说明应保留原始原因：%q", out.Detail)
	}
}

// 受理成功后卡片转成「更新中」，并说明在哪个节点上重建。
func TestRemoteUpdateStartsOnNode(t *testing.T) {
	r := &fakeRemote{online: true}
	s, st := newRemoteService(t, r)
	n := createRemoteNotification(t, st, 7)

	out := s.applyRemoteUpdate(n)
	if out.State != model.CardUpdating {
		t.Errorf("受理后应进入更新中，实际 %q", out.State)
	}
	if len(r.calls) != 1 || r.calls[0] != "c1" {
		t.Errorf("应把容器编号下发到该节点，实际 %v", r.calls)
	}
	if !strings.Contains(out.Detail, "1panel") {
		t.Errorf("受理说明里要写清是哪台机器：%q", out.Detail)
	}
}

// 远端更新结束时，卡片状态与已读要一起改掉。
func TestNodeOpEventWritesBackCard(t *testing.T) {
	s, st := newRemoteService(t, nil)
	n := createRemoteNotification(t, st, 7)

	s.NodeOpEvent(7, hubproto.Event{
		OpID: "u-1", Kind: "update", Phase: hubproto.PhaseDone,
		ContainerID: "c1", ContainerName: "web", Message: "更新完成",
	})

	got, err := st.GetNotification(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CardState != model.CardDone {
		t.Errorf("完成事件应把卡片改成 done，实际 %q", got.CardState)
	}
	if !got.Read {
		t.Error("更新结束后应标为已读，否则未读角标会一直挂着")
	}
}

// 失败事件要把原因带进卡片。没有原因时也不能留一句空的「更新失败：」。
func TestNodeOpEventCarriesFailureReason(t *testing.T) {
	s, st := newRemoteService(t, nil)
	n := createRemoteNotification(t, st, 7)

	s.NodeOpEvent(7, hubproto.Event{
		OpID: "u-1", Kind: "update", Phase: hubproto.PhaseFailed,
		ContainerID: "c1", ContainerName: "web", Message: "健康检查超时",
	})
	got, _ := st.GetNotification(n.ID)
	if got.CardState != model.CardFailed {
		t.Errorf("失败事件应把卡片改成 failed，实际 %q", got.CardState)
	}
	// 原因必须落库：网页上没有别的地方能显示它，用户只看到一个「失败」徽章
	// 就等于什么也没得到 —— 不知道该改什么，也没法把原因贴出来。
	if !strings.Contains(got.CardDetail, "健康检查超时") {
		t.Errorf("失败原因应写进库里，实际 %q", got.CardDetail)
	}
}

// 节点离线时点更新要如实报错，不能回一句空话当成下发成功。
//
// 旧实现里 applyAction 返回的 State 是空的、只在 Toast 里写了原因，
// ApplyUpdate 只认 CardFailed，于是原样返回 ("", nil)；页面弹「已下发更新」，
// 用户以为动了，其实那条提醒一直停在原地。
func TestApplyUpdateReportsOfflineNode(t *testing.T) {
	s, st := newRemoteService(t, &fakeRemote{online: false})
	n := createRemoteNotification(t, st, 7)
	if err := st.SetNotificationCardResult(n.ID, model.CardFailed, "上次失败"); err != nil {
		t.Fatal(err)
	}
	n.CardState = model.CardFailed

	_, err := s.ApplyUpdate(n)
	if err == nil {
		t.Fatal("节点离线时应报错")
	}
	if !strings.Contains(err.Error(), "不在线") {
		t.Errorf("错误信息要说明原因，实际 %q", err)
	}
}

// 提醒里的容器编号过期时，按名字找回当前实例并把编号对齐。
//
// 容器一被重建（compose、外部升级、回滚）编号就变了，而提醒存的是发提醒那天的。
// 只认编号的话，用户点「重试」永远只会看到「找不到该容器」；就算 Hub 侧能按名字
// 找到，不把新编号写回通知，节点回传的结果也会对不上那条卡片。
func TestRemoteUpdateRealignsStaleContainerID(t *testing.T) {
	r := &fakeRemote{
		online:   true,
		resolved: &model.NodeContainer{ContainerID: "c2-new", ContainerName: "web"},
	}
	s, st := newRemoteService(t, r)
	n := createRemoteNotification(t, st, 7) // 库里记着 c1

	out := s.applyRemoteUpdate(n)
	if out.State != model.CardUpdating {
		t.Fatalf("受理后应进入更新中，实际 %q", out.State)
	}
	got, err := st.GetNotification(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContainerID != "c2-new" {
		t.Errorf("通知里的编号应对齐到实际下发的那个，实际 %q", got.ContainerID)
	}

	// 结果事件带新编号，必须还能落到这张卡上
	s.NodeOpEvent(7, hubproto.Event{
		OpID: "u-2", Kind: "update", Phase: hubproto.PhaseDone,
		ContainerID: "c2-new", ContainerName: "web", Message: "更新完成",
	})
	got, _ = st.GetNotification(n.ID)
	if got.CardState != model.CardDone {
		t.Errorf("对齐后结果应能写回卡片，实际 %q", got.CardState)
	}
}

// 老通知的编号对不上时，事件里带着容器名也要能落到那张卡上。
func TestNodeOpEventMatchesByContainerName(t *testing.T) {
	s, st := newRemoteService(t, nil)
	n := createRemoteNotification(t, st, 7) // container_id=c1, name=web

	s.NodeOpEvent(7, hubproto.Event{
		OpID: "u-3", Kind: "update", Phase: hubproto.PhaseDone,
		ContainerID: "c9", ContainerName: "web", Message: "更新完成",
	})
	got, _ := st.GetNotification(n.ID)
	if got.CardState != model.CardDone {
		t.Errorf("按容器名也应能写回结果，实际 %q", got.CardState)
	}
}

// 失败往往是一次性的（镜像源抽风、目标机磁盘满），修好之后最自然的动作
// 就是原地点一次重来。之前 failed 没有按钮、后端也只认 pending/updating，
// 用户唯一的出路是删掉记录再等下一轮巡检。
func TestApplyUpdateRetriesFailedNotification(t *testing.T) {
	r := &fakeRemote{online: true}
	s, st := newRemoteService(t, r)
	n := createRemoteNotification(t, st, 7)
	if err := st.SetNotificationCardResult(n.ID, model.CardFailed, "拉取镜像失败：1234 unauthorized"); err != nil {
		t.Fatal(err)
	}
	n.CardState = model.CardFailed

	if _, err := s.ApplyUpdate(n); err != nil {
		t.Fatalf("失败的提醒应该能重试: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("应重新下发一次指令，实际 %v", r.calls)
	}
	got, _ := st.GetNotification(n.ID)
	if got.CardState != model.CardUpdating {
		t.Errorf("重试后库里应记为 updating，实际 %q", got.CardState)
	}
	if got.CardDetail != "" {
		t.Errorf("重试后上一次的失败原因要清掉，实际 %q", got.CardDetail)
	}
}

// 中间进度不刷卡片：那些「拉取镜像、停止旧容器」会让人以为出了问题。
func TestNodeOpEventIgnoresProgress(t *testing.T) {
	s, st := newRemoteService(t, nil)
	n := createRemoteNotification(t, st, 7)

	s.NodeOpEvent(7, hubproto.Event{
		OpID: "u-1", Phase: hubproto.PhaseProgress,
		ContainerID: "c1", Message: "拉取镜像 30%",
	})
	got, _ := st.GetNotification(n.ID)
	if got.CardState != model.CardUpdating {
		t.Errorf("中间进度不该改卡片状态，实际 %q", got.CardState)
	}
}

// 节点上的「忽略」不需要写 images 表：远端去重靠通知记录本身。
// 这条测试盯的是「别去查一个不存在的监控项然后崩掉」。
func TestIgnoreRemoteNotificationDoesNotTouchImages(t *testing.T) {
	s, st := newRemoteService(t, nil)
	n := createRemoteNotification(t, st, 7)
	_ = st.SetNotificationCardState(n.ID, model.CardPending)

	out := s.applyAction(n, ActionIgnore, model.ChannelFeishu)
	if out.State != model.CardIgnored {
		t.Errorf("忽略后卡片应变成 ignored，实际 %q", out.State)
	}
	got, _ := st.GetNotification(n.ID)
	if got.CardState != model.CardIgnored {
		t.Errorf("库里的状态没改：%q", got.CardState)
	}
	time.Sleep(50 * time.Millisecond)
}

// 完整走一遍真实链路：卡片是待更新 → 点「更新」→ 节点跑完回传 done。
//
// 这条链以前是断的：applyRemoteUpdate 只把「更新中」的样子塞进回调响应，
// 数据库里仍然是 pending；等节点回传 done，NodeOpEvent 按 card_state='updating'
// 查不到任何一条通知，于是卡片永远停在「正在更新」——网页上的横幅早就绿了。
// 旧测试没抓到，是因为它建的通知一开始就被写成了 updating，把中间这一步跳过去了。
func TestRemoteUpdateThenDoneReachesCard(t *testing.T) {
	r := &fakeRemote{online: true}
	s, st := newRemoteService(t, r)
	n := createRemoteNotification(t, st, 7)
	// 从真实起点出发：提醒刚发出去时是「待更新」
	if err := st.SetNotificationCardState(n.ID, model.CardPending); err != nil {
		t.Fatal(err)
	}
	n.CardState = model.CardPending

	out := s.applyRemoteUpdate(n)
	if out.State != model.CardUpdating {
		t.Fatalf("受理后应进入更新中，实际 %q", out.State)
	}
	got, err := st.GetNotification(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CardState != model.CardUpdating {
		t.Fatalf("下发后库里应记为 updating，实际 %q —— 结果回写时就会找不到这条通知", got.CardState)
	}

	s.NodeOpEvent(7, hubproto.Event{
		OpID: "u-1", Kind: "update", Phase: hubproto.PhaseDone,
		ContainerID: "c1", ContainerName: "web", Message: "更新完成",
	})
	got, _ = st.GetNotification(n.ID)
	if got.CardState != model.CardDone {
		t.Errorf("完成事件后卡片应变成 done，实际 %q", got.CardState)
	}
}

// 卡片停在「更新中」、但那台机器其实早就没在跑了 —— 这种僵尸状态必须能收尾。
//
// 以前这里是无条件回「更新已在进行中」，用户点多少次都没用，也没有别的地方
// 能把状态改回来；一条卡住的提醒就只能等下一版新提醒盖过去。
func TestStaleUpdatingCardCanBeRetried(t *testing.T) {
	r := &fakeRemote{online: true, busy: false} // 没有在跑
	s, st := newRemoteService(t, r)
	n := createRemoteNotification(t, st, 7) // 建出来就是 updating

	out := s.applyAction(n, ActionUpdate, "")
	if out.State != model.CardUpdating {
		t.Fatalf("重试应重新下发，实际状态 %q", out.State)
	}
	if len(r.calls) != 1 || r.calls[0] != "c1" {
		t.Fatalf("应该真的发了一次指令，实际 %v", r.calls)
	}

	// 真在跑的时候不能再发一遍：那会在节点上起第二个重建
	r2 := &fakeRemote{online: true, busy: true}
	s2, st2 := newRemoteService(t, r2)
	n2 := createRemoteNotification(t, st2, 7)
	if out := s2.applyAction(n2, ActionUpdate, ""); out.Toast != "更新已在进行中" {
		t.Errorf("还在跑时应拒绝重复下发，实际 %q", out.Toast)
	}
	if len(r2.calls) != 0 {
		t.Errorf("还在跑时不该再发指令，实际 %v", r2.calls)
	}
}

// 网页上的更新入口只对「待处理 / 更新中」有意义，别的状态点了要给出明确原因。
func TestApplyUpdateRejectsOtherStates(t *testing.T) {
	s, st := newRemoteService(t, &fakeRemote{online: true})
	n := createRemoteNotification(t, st, 7)
	if err := st.SetNotificationCardState(n.ID, model.CardDone); err != nil {
		t.Fatal(err)
	}
	n.CardState = model.CardDone
	if _, err := s.ApplyUpdate(n); err == nil {
		t.Error("已完成的卡片不该能再更新")
	}
}

// 「待处理」是网页上最常见的那种，它必须能直接点。
func TestApplyUpdateWorksOnPending(t *testing.T) {
	r := &fakeRemote{online: true}
	s, st := newRemoteService(t, r)
	n := createRemoteNotification(t, st, 7)
	if err := st.SetNotificationCardState(n.ID, model.CardPending); err != nil {
		t.Fatal(err)
	}
	n.CardState = model.CardPending

	if _, err := s.ApplyUpdate(n); err != nil {
		t.Fatalf("待处理的提醒应该能更新: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("应该向节点下发一次指令，实际 %v", r.calls)
	}
	got, _ := st.GetNotification(n.ID)
	if got.CardState != model.CardUpdating {
		t.Errorf("下发后库里应记为 updating，实际 %q", got.CardState)
	}
}

// 真在跑的时候点，不能起第二个重建。
func TestApplyUpdateRefusesWhileRunning(t *testing.T) {
	r := &fakeRemote{online: true, busy: true}
	s, st := newRemoteService(t, r)
	n := createRemoteNotification(t, st, 7) // updating

	if _, err := s.ApplyUpdate(n); err == nil {
		t.Fatal("还在跑的时候应该拒绝")
	}
	if len(r.calls) != 0 {
		t.Errorf("不该再发指令，实际 %v", r.calls)
	}
}

// 合并提醒点一次，按顺序把项目里的容器逐个更新完。
func TestBatchUpdateRunsMembersInOrder(t *testing.T) {
	r := &fakeRemote{online: true, doneOnDispatch: true}
	s, st := newRemoteService(t, r)

	n := &model.Notification{
		ImageRef: "chaitin/safeline-chaos:9.4.3", NodeID: 7, NodeName: "1panel",
		ComposeProject: "safeline", Kind: model.NotifNewVersion, CardState: model.CardPending,
	}
	id, err := st.CreateNotificationWithMembers(n, []model.NotificationMember{
		{ContainerID: "c1", ContainerName: "safeline-chaos", ImageRef: "chaitin/safeline-chaos:9.4.3"},
		{ContainerID: "c2", ContainerName: "safeline-fvm", ImageRef: "chaitin/safeline-fvm:9.4.3"},
		{ContainerID: "c3", ContainerName: "safeline-mgt", ImageRef: "chaitin/safeline-mgt:9.4.3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetNotification(id)
	if err != nil {
		t.Fatal(err)
	}

	out := s.startBatch(got)
	if out.State != model.CardUpdating {
		t.Fatalf("受理后应进入更新中，实际 %q（%s）", out.State, out.Detail)
	}

	// 等批处理收尾
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cur, _ := st.GetNotification(id)
		if cur.CardState != model.CardUpdating {
			got = cur
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.CardState != model.CardDone {
		t.Fatalf("三个都成功时卡片应完成，实际 %q（%s）", got.CardState, got.CardDetail)
	}
	if len(r.calls) != 3 {
		t.Fatalf("应逐个下发 3 次，实际 %v", r.calls)
	}
	if len(r.preloadCalls) != 3 {
		t.Fatalf("重建之前应先把 3 个镜像都拉一遍，实际 %v", r.preloadCalls)
	}
	for _, m := range got.Members {
		if m.State != "done" {
			t.Errorf("成员 %s 应记为完成，实际 %q", m.ContainerName, m.State)
		}
	}
}

// 中间一个失败就停下：后面的容器不动，卡片写明停在哪一个。
func TestBatchUpdateStopsOnFirstFailure(t *testing.T) {
	r := &fakeRemote{online: true, failOnDispatch: "拉取镜像失败: i/o timeout"}
	s, st := newRemoteService(t, r)

	n := &model.Notification{
		ImageRef: "chaitin/safeline-chaos:9.4.3", NodeID: 7, NodeName: "1panel",
		ComposeProject: "safeline", Kind: model.NotifNewVersion, CardState: model.CardPending,
	}
	id, err := st.CreateNotificationWithMembers(n, []model.NotificationMember{
		{ContainerID: "c1", ContainerName: "safeline-chaos", ImageRef: "chaitin/safeline-chaos:9.4.3"},
		{ContainerID: "c2", ContainerName: "safeline-fvm", ImageRef: "chaitin/safeline-fvm:9.4.3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetNotification(id)

	s.startBatch(got)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cur, _ := st.GetNotification(id)
		if cur.CardState != model.CardUpdating {
			got = cur
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.CardState != model.CardFailed {
		t.Fatalf("有成员失败时卡片应失败，实际 %q", got.CardState)
	}
	if len(r.calls) != 1 {
		t.Errorf("失败后不该继续下发，实际 %v", r.calls)
	}
	if !strings.Contains(got.CardDetail, "safeline-chaos") || !strings.Contains(got.CardDetail, "timeout") {
		t.Errorf("结果说明要写清停在哪一个、为什么：%q", got.CardDetail)
	}
}

// 预拉阶段失败：一个容器都不该重建，整批原样不动。
//
// 这是「先全拉再重建」的全部意义。先前逐个「拉+建」，第二个成员拉不到的时候
// 第一个已经换成新版了，整个 compose 项目停在新旧混搭上 —— 旧的回不去，
// 新的互相起不来，比不更新更难收拾。
func TestBatchUpdatePreloadFailureTouchesNothing(t *testing.T) {
	r := &fakeRemote{
		online:     true,
		preloadErr: errors.New("镜像源拒绝访问（HTTP 403）：免费节点当前繁忙，请稍后重试"),
	}
	s, st := newRemoteService(t, r)

	n := &model.Notification{
		ImageRef: "chaitin/safeline-chaos:9.4.3", NodeID: 7, NodeName: "1panel",
		ComposeProject: "safeline", Kind: model.NotifNewVersion, CardState: model.CardPending,
	}
	id, err := st.CreateNotificationWithMembers(n, []model.NotificationMember{
		{ContainerID: "c1", ContainerName: "safeline-chaos", ImageRef: "chaitin/safeline-chaos:9.4.3"},
		{ContainerID: "c2", ContainerName: "safeline-fvm", ImageRef: "chaitin/safeline-fvm:9.4.3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetNotification(id)

	s.startBatch(got)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cur, _ := st.GetNotification(id)
		if cur.CardState != model.CardUpdating {
			got = cur
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got.CardState != model.CardFailed {
		t.Fatalf("预拉失败时卡片应失败，实际 %q（%s）", got.CardState, got.CardDetail)
	}
	if len(r.calls) != 0 {
		t.Fatalf("预拉失败时一个容器都不该重建，实际下发了 %v", r.calls)
	}
	for _, m := range got.Members {
		if m.State != "pending" {
			t.Errorf("容器没动过，成员 %s 应仍为待处理，实际 %q", m.ContainerName, m.State)
		}
	}
	for _, want := range []string{"一个都没有改动", "免费节点当前繁忙"} {
		if !strings.Contains(got.CardDetail, want) {
			t.Errorf("结果说明应包含 %q：%q", want, got.CardDetail)
		}
	}
}

// 老 Agent 不认识 pull：退回逐个更新，不能把整批判失败。
//
// 版本差不是故障。节点会自动跟随升级，但升级完成前那段时间里，
// 一次「点了就报不支持」足以让人以为工具坏了。
func TestBatchUpdateFallsBackWhenPullUnsupported(t *testing.T) {
	r := &fakeRemote{online: true, doneOnDispatch: true, pullUnsupported: true}
	s, st := newRemoteService(t, r)

	n := &model.Notification{
		ImageRef: "chaitin/safeline-chaos:9.4.3", NodeID: 7, NodeName: "1panel",
		ComposeProject: "safeline", Kind: model.NotifNewVersion, CardState: model.CardPending,
	}
	id, err := st.CreateNotificationWithMembers(n, []model.NotificationMember{
		{ContainerID: "c1", ContainerName: "safeline-chaos", ImageRef: "chaitin/safeline-chaos:9.4.3"},
		{ContainerID: "c2", ContainerName: "safeline-fvm", ImageRef: "chaitin/safeline-fvm:9.4.3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetNotification(id)

	s.startBatch(got)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cur, _ := st.GetNotification(id)
		if cur.CardState != model.CardUpdating {
			got = cur
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got.CardState != model.CardDone {
		t.Fatalf("老 Agent 上应当照旧逐个更新成功，实际 %q（%s）", got.CardState, got.CardDetail)
	}
	if len(r.calls) != 2 {
		t.Errorf("两个容器都该被更新，实际 %v", r.calls)
	}
}

// 预拉必须「拉完一个再发下一个」。
//
// 真 Agent 一把锁：回执「开始拉取镜像」只代表受理，镜像还在往机器上写。
// 老代码拿到回执就去发下一个成员的 pull，当场被挡成「已有更新任务正在执行」，
// 整批判失败 —— 而实际上第一个镜像正在正常下载。用户看到的就是
// 「明明在拉，却报错，还让我换加速源」。
func TestBatchPreloadWaitsForEachPull(t *testing.T) {
	r := &fakeRemote{online: true, doneOnDispatch: true, pullFor: 60 * time.Millisecond}
	s, st := newRemoteService(t, r)

	n := &model.Notification{
		ImageRef: "chaitin/safeline-fvm:9.4.3", NodeID: 7, NodeName: "1panel",
		ComposeProject: "safeline", Kind: model.NotifNewVersion, CardState: model.CardPending,
	}
	id, err := st.CreateNotificationWithMembers(n, []model.NotificationMember{
		{ContainerID: "c1", ContainerName: "safeline-fvm", ImageRef: "chaitin/safeline-fvm:9.4.3"},
		{ContainerID: "c2", ContainerName: "safeline-mgt", ImageRef: "chaitin/safeline-mgt:9.4.3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetNotification(id)

	s.startBatch(got)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		cur, _ := st.GetNotification(id)
		if cur.CardState != model.CardUpdating {
			got = cur
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got.CardState != model.CardDone {
		t.Fatalf("两个镜像都拉得下来，整批应当成功，实际 %q（%s）", got.CardState, got.CardDetail)
	}
	if len(r.preloadCalls) != 2 {
		t.Errorf("两个成员都该预拉，实际 %v", r.preloadCalls)
	}
	if len(r.calls) != 2 {
		t.Errorf("预拉成功后两个都该重建，实际 %v", r.calls)
	}
}

// 撞上另一把锁时，不能把原因栽到镜像源头上。
//
// 「拉取镜像失败，换个加速源」只对真的拉不到成立。若是这台机器上还有一条
// 指令在拉镜像，换多少加速源都没用 —— 那句话会让人反复换源、反复失败。
func TestBatchPreloadBusyDoesNotBlameMirror(t *testing.T) {
	r := &fakeRemote{online: true, doneOnDispatch: true}
	// 另一条指令正在拉镜像（网页上刚点的一批）。
	r.mu.Lock()
	r.pulling = true
	r.mu.Unlock()

	s, st := newRemoteService(t, r)
	n := &model.Notification{
		ImageRef: "chaitin/safeline-fvm:9.4.3", NodeID: 7, NodeName: "1panel",
		ComposeProject: "safeline", Kind: model.NotifNewVersion, CardState: model.CardPending,
	}
	id, err := st.CreateNotificationWithMembers(n, []model.NotificationMember{
		{ContainerID: "c1", ContainerName: "safeline-fvm", ImageRef: "chaitin/safeline-fvm:9.4.3"},
		{ContainerID: "c2", ContainerName: "safeline-mgt", ImageRef: "chaitin/safeline-mgt:9.4.3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetNotification(id)

	s.startBatch(got)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cur, _ := st.GetNotification(id)
		if cur.CardState != model.CardUpdating {
			got = cur
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got.CardState != model.CardFailed {
		t.Fatalf("撞锁时这批不算开始，卡片应标失败，实际 %q", got.CardState)
	}
	if len(r.preloadCalls) != 0 || len(r.calls) != 0 {
		t.Errorf("一条指令都不该落地，实际预拉 %v / 重建 %v", r.preloadCalls, r.calls)
	}
	for _, want := range []string{"一个都没有改动", "1panel", "这跟镜像源无关"} {
		if !strings.Contains(got.CardDetail, want) {
			t.Errorf("结果说明应包含 %q：%q", want, got.CardDetail)
		}
	}
	if strings.Contains(got.CardDetail, "换个镜像加速源") {
		t.Errorf("撞锁不该劝人换加速源：%q", got.CardDetail)
	}
}

// 卡片上的「一键更新」也要按 depends_on 排顺序。
//
// 与节点页那条批量是同一批容器、同一个风险：下游先重建就会在上游重启的
// 几秒里连不上。两条路径各写一遍排序上下文（一条拿快照、一条拿成员），
// 所以两条都要有回归测试，漏一条就等于那个入口永远不排。
func TestBatchFollowsComposeOrder(t *testing.T) {
	r := &fakeRemote{online: true, doneOnDispatch: true}
	s, st := newRemoteService(t, r)

	// 快照按名字序，tengine 在最前。
	if err := st.ReplaceNodeContainers(7, []model.NodeContainer{
		{NodeID: 7, ContainerID: "c3", ContainerName: "safeline-tengine", ImageRef: "chaitin/tengine:9.4.3",
			ComposeProject: "safeline", ComposeService: "safeline-tengine", ComposeDeps: []string{"safeline-mgt"}},
		{NodeID: 7, ContainerID: "c1", ContainerName: "safeline-pg", ImageRef: "chaitin/pg:9.4.3",
			ComposeProject: "safeline", ComposeService: "safeline-pg"},
		{NodeID: 7, ContainerID: "c2", ContainerName: "safeline-mgt", ImageRef: "chaitin/mgt:9.4.3",
			ComposeProject: "safeline", ComposeService: "safeline-mgt", ComposeDeps: []string{"safeline-pg"}},
	}); err != nil {
		t.Fatal(err)
	}

	n := &model.Notification{
		ImageRef: "chaitin/safeline-tengine:9.4.3", NodeID: 7, NodeName: "1panel",
		ComposeProject: "safeline", Kind: model.NotifNewVersion, CardState: model.CardPending,
	}
	id, err := st.CreateNotificationWithMembers(n, []model.NotificationMember{
		{ContainerID: "c3", ContainerName: "safeline-tengine", ImageRef: "chaitin/tengine:9.4.3"},
		{ContainerID: "c1", ContainerName: "safeline-pg", ImageRef: "chaitin/pg:9.4.3"},
		{ContainerID: "c2", ContainerName: "safeline-mgt", ImageRef: "chaitin/mgt:9.4.3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetNotification(id)

	s.startBatch(got)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		cur, _ := st.GetNotification(id)
		if cur.CardState != model.CardUpdating {
			got = cur
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.CardState != model.CardDone {
		t.Fatalf("三个都该成功，实际 %q（%s）", got.CardState, got.CardDetail)
	}

	want := []string{"c1", "c2", "c3"}
	if len(r.preloadCalls) != 3 {
		t.Fatalf("三个都该预拉，实际 %v", r.preloadCalls)
	}
	for i := range want {
		if r.preloadCalls[i] != want[i] {
			t.Fatalf("预拉顺序没按依赖排：期望 %v，实际 %v", want, r.preloadCalls)
		}
		if r.calls[i] != want[i] {
			t.Fatalf("重建顺序没按依赖排：期望 %v，实际 %v", want, r.calls)
		}
	}
}

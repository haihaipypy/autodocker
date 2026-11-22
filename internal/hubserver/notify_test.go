package hubserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/haihaipypy/autodocker/internal/hubproto"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/store"
)

// fakeNotifier 记录「推了几条、推的是什么」，不做任何网络动作。
type fakeNotifier struct {
	mu   sync.Mutex
	got  []model.Notification
	when chan struct{}
}

func (f *fakeNotifier) Notify(_ context.Context, n *model.Notification) (string, error) {
	f.mu.Lock()
	f.got = append(f.got, *n)
	f.mu.Unlock()
	if f.when != nil {
		select {
		case f.when <- struct{}{}:
		default:
		}
	}
	// 模拟一个渠道成功投递，验证投递坐标会被写回库
	n.Deliveries = []model.Delivery{{Channel: model.ChannelFeishu, ChatRef: "chat", MessageID: "msg-1"}}
	return "msg-1", nil
}

func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

func (f *fakeNotifier) last() (model.Notification, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) == 0 {
		return model.Notification{}, false
	}
	return f.got[len(f.got)-1], true
}

// newNotifyHub 起一个带通知回调的 Hub。
func newNotifyHub(t *testing.T) (*Server, *store.Store, *httptest.Server, int64, *fakeNotifier) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	id, err := st.CreateNode(&model.Node{Name: "1panel", HubURL: "https://docker.1day.vip"}, tokenHash("secret-token"))
	if err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}

	nf := &fakeNotifier{when: make(chan struct{}, 4)}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(Config{Version: "test-1.8.2", Notifier: nf}, st, logger)
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleWS))
	t.Cleanup(ts.Close)
	return srv, st, ts, id, nf
}

func reportContainers(t *testing.T, c interface {
	WriteJSON(v any) error
}, list ...hubproto.Container) {
	t.Helper()
	raw, _ := json.Marshal(hubproto.Report{ScannedAt: time.Now().Unix(), Containers: list})
	if err := c.WriteJSON(hubproto.Frame{Type: hubproto.TypeReport, Data: raw}); err != nil {
		t.Fatalf("发送上报失败: %v", err)
	}
}

func waitNotifications(t *testing.T, st *store.Store, want int) []model.Notification {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		list, err := st.ListNotifications(false, 50)
		if err == nil && len(list) >= want {
			return list
		}
		time.Sleep(20 * time.Millisecond)
	}
	list, _ := st.ListNotifications(false, 50)
	t.Fatalf("等待通知落库超时：期望 %d 条，实际 %d 条", want, len(list))
	return nil
}

// 远端发现更新要变成一条带节点信息的提醒，而且只推一次。
func TestReportCreatesNodeNotificationOnce(t *testing.T) {
	_, st, ts, nodeID, nf := newNotifyHub(t)

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	if f := sendHello(t, c, hubproto.Protocol); f.Type != hubproto.TypeHelloOK {
		t.Fatalf("握手失败: %s", f.Type)
	}

	rep := hubproto.Container{
		ContainerID: "c1", ContainerName: "cloud-saver", ImageRef: "jiangrui1994/cloudsaver:latest",
		LocalTag: "latest", LocalDigest: "sha256:aaa", RemoteDigest: "sha256:bbb",
		Origin: "container", Status: "update_available", Mode: "digest-only",
	}
	reportContainers(t, c, rep)

	list := waitNotifications(t, st, 1)
	if list[0].NodeID != nodeID {
		t.Errorf("通知应记下节点 ID %d，实际 %d", nodeID, list[0].NodeID)
	}
	if list[0].NodeName != "1panel" {
		t.Errorf("通知应记下节点名，实际 %q", list[0].NodeName)
	}
	if !strings.Contains(list[0].Title, "1panel") {
		t.Errorf("标题里要带节点名，否则分不清是哪台机器：%q", list[0].Title)
	}

	// 推送是异步的，等它跑完再断言
	select {
	case <-nf.when:
	case <-time.After(2 * time.Second):
		t.Fatal("通知没有推送给渠道")
	}
	if n, ok := nf.last(); !ok || n.NodeID != nodeID {
		t.Errorf("推给渠道的通知不对: %+v", n)
	}

	// 同一轮的下一份上报：内容没变，不能再打扰一次
	reportContainers(t, c, rep)
	time.Sleep(300 * time.Millisecond)
	if got := len(waitNotifications(t, st, 1)); got != 1 {
		t.Errorf("同一版本不该重复提醒，实际有 %d 条通知", got)
	}
}

// 远端内容真的变了（浮动 tag 换了摘要）时，要能再提醒一次。
func TestReportNotifiesAgainWhenDigestChanges(t *testing.T) {
	_, st, ts, _, _ := newNotifyHub(t)

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	if f := sendHello(t, c, hubproto.Protocol); f.Type != hubproto.TypeHelloOK {
		t.Fatalf("握手失败: %s", f.Type)
	}

	base := hubproto.Container{
		ContainerID: "c1", ContainerName: "web", ImageRef: "nginx:latest",
		LocalTag: "latest", LocalDigest: "sha256:aaa", RemoteDigest: "sha256:bbb",
		Origin: "container", Status: "update_available", Mode: "digest-only",
	}
	reportContainers(t, c, base)
	waitNotifications(t, st, 1)

	base.RemoteDigest = "sha256:ccc"
	reportContainers(t, c, base)
	list := waitNotifications(t, st, 2)
	if len(list) != 2 {
		t.Fatalf("新内容应再提醒一次，实际 %d 条", len(list))
	}
}

// 节点进度要落成 Hub 侧的任务记录（带节点），并回调给通知服务。
func TestNodeEventsCreateTaskAndNotify(t *testing.T) {
	srv, st, ts, nodeID, _ := newNotifyHub(t)

	var (
		mu   sync.Mutex
		seen []hubproto.Event
	)
	srv.cfg.OnOp = func(_ int64, ev hubproto.Event) {
		mu.Lock()
		seen = append(seen, ev)
		mu.Unlock()
	}

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	if f := sendHello(t, c, hubproto.Protocol); f.Type != hubproto.TypeHelloOK {
		t.Fatalf("握手失败: %s", f.Type)
	}

	// 先上报一次，任务行才能补上镜像引用
	reportContainers(t, c, hubproto.Container{
		ContainerID: "c1", ContainerName: "web", ImageRef: "nginx:1.24",
		Origin: "container", Status: "new_version_tag", LatestTag: "1.27",
	})
	waitContainers(t, st, nodeID, 1)

	sendEvent := func(phase, msg string) {
		raw, _ := json.Marshal(hubproto.Event{
			OpID: "u-1", Kind: "update", Phase: phase,
			ContainerID: "c1", ContainerName: "web", Message: msg,
			TaskID: 42, At: time.Now().Unix(),
		})
		if err := c.WriteJSON(hubproto.Frame{Type: hubproto.TypeEvent, Data: raw}); err != nil {
			t.Fatalf("发送进度帧失败: %v", err)
		}
	}
	sendEvent(hubproto.PhaseStart, "开始更新 web")
	sendEvent(hubproto.PhaseProgress, "拉取 nginx:1.27")
	sendEvent(hubproto.PhaseDone, "更新完成")

	// 等任务落成 success
	deadline := time.Now().Add(3 * time.Second)
	var task *model.Task
	for time.Now().Before(deadline) {
		list, _ := st.ListTasks(10)
		if len(list) > 0 && list[0].Status == model.TaskSuccess {
			task = &list[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if task == nil {
		list, _ := st.ListTasks(10)
		t.Fatalf("远端更新没有生成成功任务，当前任务: %+v", list)
	}
	if task.NodeID != nodeID || task.NodeName != "1panel" {
		t.Errorf("任务没记下节点: %+v", task)
	}
	if task.ImageRef != "nginx:1.24" {
		t.Errorf("任务应补上镜像引用，实际 %q", task.ImageRef)
	}
	if !strings.Contains(task.Log, "拉取 nginx:1.27") {
		t.Errorf("进度日志没写进任务: %q", task.Log)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("三个进度帧都应回调，实际 %d 次", len(seen))
	}
	if seen[len(seen)-1].Phase != hubproto.PhaseDone {
		t.Errorf("最后一次回调应是 done，实际 %q", seen[len(seen)-1].Phase)
	}
}

// 去重键：有更高 tag 用 tag，浮动 tag 用摘要，且两者不能串味。
func TestNodeVersionKey(t *testing.T) {
	withTag := model.NodeContainer{LatestTag: "1.27", RemoteDigest: "sha256:bbb"}
	if got := nodeVersionKey(withTag); got != "tag:1.27" {
		t.Errorf("有更高 tag 时应以 tag 为准，实际 %q", got)
	}
	noTag := model.NodeContainer{RemoteDigest: "sha256:bbb"}
	if got := nodeVersionKey(noTag); got != "digest:sha256:bbb" {
		t.Errorf("浮动 tag 应退回摘要，实际 %q", got)
	}
	if nodeVersionKey(model.NodeContainer{}) != "" {
		t.Error("两者都没有时应返回空，避免把「不知道变没变」当成同一版")
	}
}

// 同一 compose 项目里的多个服务只推一条提醒、只发一条消息。
//
// 之前是一个容器一条：safeline 一次发 6 个服务就是 6 条消息，手机上弹一屏，
// 还得挨个点「更新」。合并之后一条提醒覆盖整个项目，一次点击依次重建。
func TestReportGroupsComposeProject(t *testing.T) {
	_, st, ts, _, nf := newNotifyHub(t)

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	if f := sendHello(t, c, hubproto.Protocol); f.Type != hubproto.TypeHelloOK {
		t.Fatalf("握手失败: %s", f.Type)
	}

	var reps []hubproto.Container
	for i, name := range []string{"chaos", "fvm", "mgt"} {
		reps = append(reps, hubproto.Container{
			ContainerID: "c" + string(rune('1'+i)), ContainerName: "safeline-" + name,
			ComposeProject: "safeline",
			ImageRef:       "chaitin/safeline-" + name + ":9.4.3",
			LocalTag:       "9.4.3", LocalDigest: "sha256:old", RemoteDigest: "sha256:new",
			Origin: "container", Status: "update_available", Mode: "digest-only",
		})
	}
	reportContainers(t, c, reps...)

	list := waitNotifications(t, st, 1)
	if len(list) != 1 {
		t.Fatalf("同项目应只落一条通知，实际 %d 条", len(list))
	}
	n := list[0]
	if n.MemberCount != 3 || len(n.Members) != 3 {
		t.Errorf("一条通知应覆盖 3 个容器，实际 count=%d members=%d", n.MemberCount, len(n.Members))
	}
	if !strings.Contains(n.Title, "safeline（3 个容器）") {
		t.Errorf("标题应写明项目与容器数，实际 %q", n.Title)
	}

	// 推送是异步的：三条容器只该发一条消息
	select {
	case <-nf.when:
	case <-time.After(2 * time.Second):
		t.Fatal("合并提醒没有推送")
	}
	time.Sleep(200 * time.Millisecond)
	if got := nf.count(); got != 1 {
		t.Errorf("三个容器应只推一条消息，实际 %d 条", got)
	}

	// 下一轮同样的快照：成员表去重要挡住，不能再推一遍
	reportContainers(t, c, reps...)
	time.Sleep(300 * time.Millisecond)
	if got := nf.count(); got != 1 {
		t.Errorf("同一版本不该重复推送，实际累计 %d 条", got)
	}
}

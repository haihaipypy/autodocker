package hubserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/haihaipypy/autodocker/internal/hubproto"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/store"
)

func tokenHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func newTestHub(t *testing.T) (*Server, *store.Store, *httptest.Server, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	id, err := st.CreateNode(&model.Node{Name: "群晖", HubURL: "http://192.168.2.10:19999"}, tokenHash("secret-token"))
	if err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(Config{Version: "test-1.7.0", ScanInterval: 0}, st, logger)
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleWS))
	t.Cleanup(ts.Close)
	return srv, st, ts, id
}

func dialAgent(t *testing.T, ts *httptest.Server, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	return websocket.DefaultDialer.Dial(u, hdr)
}

func sendHello(t *testing.T, c *websocket.Conn, proto int) hubproto.Frame {
	t.Helper()
	raw, _ := json.Marshal(hubproto.Hello{
		Hostname: "nas1", OS: "fnOS (Debian)", Arch: "amd64",
		AgentVersion: "1.7.0", Protocol: proto, NodeName: "群晖",
	})
	if err := c.WriteJSON(hubproto.Frame{Type: hubproto.TypeHello, Data: raw}); err != nil {
		t.Fatalf("发送注册帧失败: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var f hubproto.Frame
	if err := c.ReadJSON(&f); err != nil {
		t.Fatalf("读取握手回执失败: %v", err)
	}
	return f
}

// 一次完整的注册与上报：这是 v1.7 的主链路，断了就什么都看不见。
func TestAgentRegistersAndReports(t *testing.T) {
	_, st, ts, nodeID := newTestHub(t)

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()

	f := sendHello(t, c, hubproto.Protocol)
	if f.Type != hubproto.TypeHelloOK {
		t.Fatalf("应收到 hello_ok，实际 %s（%s）", f.Type, f.Error)
	}
	var ok hubproto.HelloOK
	_ = json.Unmarshal(f.Data, &ok)
	if ok.NodeID != nodeID {
		t.Errorf("回执里的节点 ID 不对: 期望 %d，实际 %d", nodeID, ok.NodeID)
	}
	if ok.HubVersion != "test-1.7.0" {
		t.Errorf("回执应带上 Hub 版本，实际 %q", ok.HubVersion)
	}

	// 注册信息要落库：系统名来自 Agent 上报的 docker info，
	// Hub 自己看不到目标机的发行版。
	n, err := st.GetNode(nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if n.Hostname != "nas1" || n.OS != "fnOS (Debian)" {
		t.Errorf("注册信息没写对: %+v", n)
	}
	if n.Status != model.NodeOnline {
		t.Errorf("连上后应标为在线，实际 %s", n.Status)
	}

	raw, _ := json.Marshal(hubproto.Report{
		ScannedAt: time.Now().Unix(),
		Containers: []hubproto.Container{
			{
				ContainerID: "c1", ContainerName: "web", ImageRef: "nginx:latest",
				LocalTag: "latest", RemoteDigest: "sha256:bbb", LatestTag: "1.27",
				Origin: "container", Status: "update_available", Mode: "digest-only",
			},
		},
		Logs: []string{"启动完成"},
	})
	if err := c.WriteJSON(hubproto.Frame{Type: hubproto.TypeReport, Data: raw}); err != nil {
		t.Fatalf("发送上报失败: %v", err)
	}

	// 读到即处理，但落库在服务端协程里，给它一点时间
	var list []model.NodeContainer
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		list, _ = st.ListNodeContainers(nodeID)
		if len(list) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(list) != 1 {
		t.Fatalf("上报的容器没有落库，实际 %d 条", len(list))
	}
	if list[0].ContainerName != "web" || list[0].LatestTag != "1.27" {
		t.Errorf("落库内容不对: %+v", list[0])
	}
	if list[0].Status != model.StatusUpdatePending {
		t.Errorf("状态应解析成 update_available，实际 %q", list[0].Status)
	}
}

// 没有令牌、令牌错、令牌被换掉 —— 一律 401，且不能提示「是哪种错」。
func TestAgentRejectsBadToken(t *testing.T) {
	_, _, ts, _ := newTestHub(t)

	for _, tok := range []string{"", "wrong-token"} {
		c, resp, err := dialAgent(t, ts, tok)
		if err == nil {
			c.Close()
			t.Fatalf("令牌 %q 竟然连上了", tok)
		}
		if resp == nil {
			t.Fatalf("令牌 %q 被拒但没有返回 HTTP 响应", tok)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("令牌 %q 应返回 401，实际 %d", tok, resp.StatusCode)
		}
	}
}

// 协议版本对不上时必须明确拒绝。用旧结构硬解新数据会出一堆
// 「看起来连上了、但状态永远是空」的怪事，比直接断开难查得多。
func TestAgentRejectsProtocolMismatch(t *testing.T) {
	_, _, ts, _ := newTestHub(t)

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()

	f := sendHello(t, c, hubproto.Protocol+99)
	if f.Type != hubproto.TypeFatal {
		t.Fatalf("版本不兼容时应回 fatal，实际 %s", f.Type)
	}
	if !strings.Contains(f.Error, "协议版本") {
		t.Errorf("错误信息应说明是协议版本问题，实际 %q", f.Error)
	}
}

// 下发指令要真的到得了对面，否则 v1.8 的「点按钮更新」无从谈起。
func TestSendCommandReachesAgent(t *testing.T) {
	srv, _, ts, nodeID := newTestHub(t)

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	if f := sendHello(t, c, hubproto.Protocol); f.Type != hubproto.TypeHelloOK {
		t.Fatalf("握手失败: %s", f.Type)
	}

	// 注册是先登记再发回执的，走到这里 conns 里已经有了
	if err := srv.Send(nodeID, hubproto.ActionScan, hubproto.Cmd{Force: true}); err != nil {
		t.Fatalf("下发指令失败: %v", err)
	}

	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var f hubproto.Frame
	if err := c.ReadJSON(&f); err != nil {
		t.Fatalf("没有收到指令: %v", err)
	}
	if f.Type != hubproto.TypeCmd || f.Action != hubproto.ActionScan {
		t.Fatalf("收到的帧不对: %+v", f)
	}
	if !strings.Contains(string(f.Data), `"force":true`) {
		t.Errorf("指令参数丢了: %s", string(f.Data))
	}

	// 不在线时必须直接报错，不能假装成功
	if err := srv.Send(9999, hubproto.ActionScan, nil); err == nil {
		t.Error("给不在线的节点发指令应报错")
	}
}

// 巡检失败且一条数据都没有时，要保留上一轮快照。
// 否则 Docker 抖一下，页面上整台机器的容器就凭空消失了。
func TestFailedReportKeepsPreviousSnapshot(t *testing.T) {
	srv, st, ts, nodeID := newTestHub(t)

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	sendHello(t, c, hubproto.Protocol)

	good, _ := json.Marshal(hubproto.Report{
		ScannedAt:  time.Now().Unix(),
		Containers: []hubproto.Container{{ContainerID: "c1", ContainerName: "web", Origin: "container"}},
	})
	_ = c.WriteJSON(hubproto.Frame{Type: hubproto.TypeReport, Data: good})
	waitContainers(t, st, nodeID, 1)

	bad, _ := json.Marshal(hubproto.Report{ScanError: "docker daemon unreachable"})
	_ = c.WriteJSON(hubproto.Frame{Type: hubproto.TypeReport, Data: bad})

	// 给服务端足够时间处理这条失败上报
	time.Sleep(200 * time.Millisecond)
	list, _ := st.ListNodeContainers(nodeID)
	if len(list) != 1 {
		t.Errorf("巡检失败时不该清空快照，实际剩 %d 条", len(list))
	}
	_ = srv
}

func waitContainers(t *testing.T, st *store.Store, nodeID int64, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		list, _ := st.ListNodeContainers(nodeID)
		if len(list) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	list, _ := st.ListNodeContainers(nodeID)
	t.Fatalf("等待快照落库超时：期望 %d 条，实际 %d 条", want, len(list))
}

// replyAfter 读一条指令，按给定的结果回执。用来模拟 Agent 那一侧。
func replyAfter(t *testing.T, c *websocket.Conn, ok bool, msg string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var f hubproto.Frame
	if err := c.ReadJSON(&f); err != nil {
		t.Errorf("Agent 侧没有收到指令: %v", err)
		return
	}
	raw, _ := json.Marshal(hubproto.CmdAck{Message: msg})
	_ = c.WriteJSON(hubproto.Frame{Type: hubproto.TypeCmdAck, ID: f.ID, OK: ok, Error: msg, Data: raw})
}

// SendWait 要真的等到回执，否则页面上「点更新」只能靠猜。
func TestSendWaitReturnsAckMessage(t *testing.T) {
	srv, _, ts, nodeID := newTestHub(t)

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	sendHello(t, c, hubproto.Protocol)

	go replyAfter(t, c, true, "已开始更新")
	msg, err := srv.SendWait(nodeID, hubproto.ActionUpdate,
		hubproto.Cmd{ContainerID: "c1", ContainerName: "web"}, 3*time.Second)
	if err != nil {
		t.Fatalf("等回执失败: %v", err)
	}
	if msg != "已开始更新" {
		t.Errorf("回执文案没带回来，实际 %q", msg)
	}
}

// 旧 Agent 会 ack 一句「不支持的指令」。原文必须原样回到 API 层，
// 由它翻译成「去目标机重跑安装命令」—— 这里只是不能把它当成成功。
func TestSendWaitSurfacesAgentError(t *testing.T) {
	srv, _, ts, nodeID := newTestHub(t)

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	sendHello(t, c, hubproto.Protocol)

	go replyAfter(t, c, false, "不支持的指令: update")
	_, err = srv.SendWait(nodeID, hubproto.ActionUpdate, hubproto.Cmd{ContainerID: "c1"}, 3*time.Second)
	if err == nil {
		t.Fatal("Agent 回了失败，SendWait 不该返回成功")
	}
	if !strings.Contains(err.Error(), "不支持的指令") {
		t.Errorf("错误文案应原样透传，实际 %q", err.Error())
	}
}

// 节点不回执时必须超时返回，不能把 API 请求永远挂住。
func TestSendWaitTimesOut(t *testing.T) {
	srv, _, ts, nodeID := newTestHub(t)

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	sendHello(t, c, hubproto.Protocol)

	start := time.Now()
	if _, err := srv.SendWait(nodeID, hubproto.ActionUpdate, hubproto.Cmd{ContainerID: "c1"}, 300*time.Millisecond); err == nil {
		t.Fatal("没人回执，应当超时报错")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("超时时间没生效，等了 %v", time.Since(start))
	}

	// 节点不在线时要立刻报错，不等超时
	if _, err := srv.SendWait(9999, hubproto.ActionUpdate, nil, time.Second); err == nil {
		t.Error("给不在线的节点下发更新应报错")
	}
}

// 进度事件要能在 Hub 侧拼出「现在在干什么」，完成态还要留一会儿给页面读到。
func TestOpStateFollowsEvents(t *testing.T) {
	srv, _, ts, nodeID := newTestHub(t)

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	sendHello(t, c, hubproto.Protocol)

	send := func(phase, msg string, taskID int64) {
		raw, _ := json.Marshal(hubproto.Event{
			OpID: "u-1", Kind: "update", Phase: phase,
			ContainerID: "c1", ContainerName: "web", Message: msg,
			TaskID: taskID, At: time.Now().Unix(),
		})
		if err := c.WriteJSON(hubproto.Frame{Type: hubproto.TypeEvent, Data: raw}); err != nil {
			t.Fatalf("发送事件失败: %v", err)
		}
	}

	send(hubproto.PhaseStart, "开始更新 web", 7)

	// 事件是异步处理的，等它落进内存
	var op *OpState
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if op = srv.Op(nodeID); op != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if op == nil {
		t.Fatal("进度事件没有记下来")
	}
	if op.Phase != hubproto.PhaseStart || op.ContainerName != "web" || op.TaskID != 7 {
		t.Errorf("操作状态不对: %+v", op)
	}

	send(hubproto.PhaseProgress, "拉取镜像 nginx:1.27", 7)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if op = srv.Op(nodeID); op != nil && op.Phase == hubproto.PhaseProgress {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if op == nil || op.Message != "拉取镜像 nginx:1.27" {
		t.Fatalf("进度文案没更新: %+v", op)
	}

	// 完成态要留着，否则用户点完按钮只会看到横幅闪一下就没了
	send(hubproto.PhaseDone, "更新完成", 7)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if op = srv.Op(nodeID); op != nil && op.Phase == hubproto.PhaseDone {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if op == nil || op.Phase != hubproto.PhaseDone {
		t.Fatalf("完成态没保留: %+v", op)
	}

	// 过了保留期必须自己消失，否则页面会永远挂着一条「更新中」
	srv.opsMu.Lock()
	srv.ops[nodeID].UpdatedAt = time.Now().Add(-2 * opDoneTTL)
	srv.opsMu.Unlock()
	if got := srv.Op(nodeID); got != nil {
		t.Errorf("过期后不该再返回操作状态: %+v", got)
	}
}

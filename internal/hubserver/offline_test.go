package hubserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/haihaipypy/autodocker/internal/hubproto"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/store"
)

// newOfflineHub 造一个只关心失联告警的 Hub：把推送收进通道，方便断言条数与内容。
func newOfflineHub(t *testing.T, wait time.Duration, on bool) (*Server, *store.Store, chan string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	msgs := make(chan string, 16)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(Config{
		Version: "test",
		OfflineAlert: func() (bool, time.Duration) {
			return on, wait
		},
		PushText: func(_ context.Context, title, body string) {
			msgs <- title + " | " + body
		},
	}, st, logger)
	// 默认把启动宽限期挪到过去：绝大多数用例测的是「判定」本身，
	// 宽限期自己单独有一个用例。
	srv.startedAt = time.Now().Add(-time.Hour)
	return srv, st, msgs
}

// makeNode 建一个节点并标成「本轮见过」，last_seen 由调用方决定。
func makeNode(t *testing.T, srv *Server, st *store.Store, name string) int64 {
	t.Helper()
	id, err := st.CreateNode(&model.Node{Name: name, HubURL: "http://hub"}, name+"-token")
	if err != nil {
		t.Fatalf("建节点失败: %v", err)
	}
	srv.markSeen(id)
	return id
}

// drain 把已经到达的消息取空。
//
// 先小睡一会儿：推送是 go routine 里发的（不能挡住巡检），立刻取会
// 在「发不出去」和「还没轮到它发」之间分不清 —— 前几个用例能过纯属侥幸。
func drain(msgs chan string) []string {
	time.Sleep(50 * time.Millisecond)
	var out []string
	for {
		select {
		case m := <-msgs:
			out = append(out, m)
		default:
			return out
		}
	}
}

// 同一次失联只报一次。
//
// 巡检每 30 秒跑一轮，不做记号的话用户会连收几十条一模一样的消息 ——
// 而他真正需要知道的只是「这台机器掉了」这一件事。
func TestOfflineAlertsOncePerOutage(t *testing.T) {
	srv, st, msgs := newOfflineHub(t, time.Nanosecond, true)
	id := makeNode(t, srv, st, "waf")
	_ = st.TouchNode(id) // 刚落过心跳，但阈值是 1 纳秒，算是超时了

	srv.checkOffline()
	first := drain(msgs)
	if len(first) != 1 {
		t.Fatalf("应当只发一条告警，实际 %d 条：%v", len(first), first)
	}
	if !strings.Contains(first[0], "节点失联：waf") {
		t.Errorf("标题要写清是哪台，实际 %q", first[0])
	}

	// 再来一轮：还在离线，但已经报过了。
	srv.checkOffline()
	if again := drain(msgs); len(again) != 0 {
		t.Errorf("同一次失联不该报第二遍，实际又发了 %d 条：%v", len(again), again)
	}
}

// Hub 刚启动的宽限期内不发任何告警。
//
// 升级 Hub 自己会重启，重启后所有节点都在重连路上。这时候按「离线」判一遍，
// 用户每次升级都会收到一屏「节点失联」—— 那条消息只能让人白紧张一次。
func TestOfflineSilentDuringStartupGrace(t *testing.T) {
	srv, st, msgs := newOfflineHub(t, time.Nanosecond, true)
	srv.startedAt = time.Now() // 刚起来
	id := makeNode(t, srv, st, "waf")
	_ = st.TouchNode(id)

	srv.checkOffline()
	if got := drain(msgs); len(got) != 0 {
		t.Fatalf("宽限期内不该告警，实际发了 %d 条：%v", len(got), got)
	}
}

// 本轮没连上过的节点不算「刚掉线」。
//
// 刚加进来还没装 Agent 的节点，last_seen 是零；从 Hub 重启到现在一直
// 没露面的节点也一样。这两类报出来都是误报。
func TestOfflineSkipsNodesNeverSeen(t *testing.T) {
	srv, st, msgs := newOfflineHub(t, time.Nanosecond, true)
	// 建了但不 markSeen：模拟「刚添加、还没连过」。
	if _, err := st.CreateNode(&model.Node{Name: "新机器", HubURL: "http://hub"}, "t2"); err != nil {
		t.Fatal(err)
	}

	srv.checkOffline()
	if got := drain(msgs); len(got) != 0 {
		t.Fatalf("从没连上过的节点不该告警，实际 %v", got)
	}
}

// 全部节点一起失联只发一条汇总。
//
// 三台机器同时断，几乎肯定是 Hub 自己的网络或出口出问题。按台数发三条，
// 用户得点开三条才能发现「哦，说的是同一件事」。
func TestOfflineAllAtOnceSendsSummary(t *testing.T) {
	srv, st, msgs := newOfflineHub(t, time.Nanosecond, true)
	for _, name := range []string{"waf", "1panel", "vps"} {
		id := makeNode(t, srv, st, name)
		_ = st.TouchNode(id)
	}

	srv.checkOffline()
	got := drain(msgs)
	if len(got) != 1 {
		t.Fatalf("全离线应只发一条汇总，实际 %d 条：%v", len(got), got)
	}
	for _, want := range []string{"3 个节点同时失联", "waf", "1panel", "vps", "Hub 这台机器"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("汇总里应包含 %q：%q", want, got[0])
		}
	}
}

// 只有一部分节点掉线时逐台报，不做汇总。
func TestOfflinePartialIsPerNode(t *testing.T) {
	srv, st, msgs := newOfflineHub(t, time.Nanosecond, true)
	down := makeNode(t, srv, st, "waf")
	_ = st.TouchNode(down)
	up := makeNode(t, srv, st, "nas")
	_ = st.TouchNode(up)

	// 把 nas 的「最后联系」往前推：它其实也在超时窗口里，但下面会把窗口调大。
	srv.cfg.OfflineAlert = func() (bool, time.Duration) { return true, time.Hour }
	// 一台很久没联系，一台刚刚联系过。
	_ = st.SetNodeLastSeen(down, time.Now().Add(-2*time.Hour))
	_ = st.TouchNode(up)

	srv.checkOffline()
	got := drain(msgs)
	if len(got) != 1 {
		t.Fatalf("只有一台超时，应当只报这一台，实际 %d 条：%v", len(got), got)
	}
	if !strings.Contains(got[0], "节点失联：waf") {
		t.Errorf("报的应该是 waf，实际 %q", got[0])
	}
}

// 节点回来后补一条「已恢复」，并把记号清掉，下一轮失联重新算。
func TestOfflineRecoveryClearsMark(t *testing.T) {
	srv, st, msgs := newOfflineHub(t, time.Nanosecond, true)
	id := makeNode(t, srv, st, "waf")
	_ = st.TouchNode(id)

	srv.checkOffline()
	if got := drain(msgs); len(got) != 1 {
		t.Fatalf("应当先发一条告警，实际 %v", got)
	}

	srv.reportRecovered(id, "waf")
	got := drain(msgs)
	if len(got) != 1 || !strings.Contains(got[0], "节点已恢复：waf") {
		t.Fatalf("应当补一条恢复消息，实际 %v", got)
	}
	if at, err := st.NodeOfflineAlertAt(id); err != nil || at != 0 {
		t.Errorf("恢复后记号要清掉，实际 at=%d err=%v", at, err)
	}

	// 重新掉线还能再报一次。
	srv.checkOffline()
	if got := drain(msgs); len(got) != 1 {
		t.Errorf("再掉一次应当重新报警，实际 %v", got)
	}
}

// 没配推送渠道时只写日志，不能因为发不出去就把状态改乱。
func TestOfflineWithoutPushStillMarks(t *testing.T) {
	srv, st, _ := newOfflineHub(t, time.Nanosecond, true)
	srv.cfg.PushText = nil
	id := makeNode(t, srv, st, "waf")
	_ = st.TouchNode(id)

	srv.checkOffline()
	if at, err := st.NodeOfflineAlertAt(id); err != nil || at == 0 {
		t.Fatalf("没有推送渠道时也要落记号，否则每轮都会重新判定，实际 at=%d err=%v", at, err)
	}
}

// 关掉开关就什么都不做。
func TestOfflineDisabled(t *testing.T) {
	srv, st, msgs := newOfflineHub(t, time.Nanosecond, false)
	id := makeNode(t, srv, st, "waf")
	_ = st.TouchNode(id)

	srv.checkOffline()
	if got := drain(msgs); len(got) != 0 {
		t.Fatalf("开关关着不该发消息，实际 %v", got)
	}
	if at, _ := st.NodeOfflineAlertAt(id); at != 0 {
		t.Errorf("开关关着不该落记号，实际 at=%d", at)
	}
}

// 握手回执要带上 Hub 配的加速地址：节点连上的那一刻就该用上，
// 而不是等第一次巡检撞一墙 401 之后再想起换源。
func TestHelloOKCarriesMirrors(t *testing.T) {
	srv, _, ts, _ := newTestHub(t)
	srv.cfg.Mirrors = func() *[]string {
		m := []string{"docker.1ms.run", "docker.xuanyuan.me"}
		return &m
	}

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
	if err := json.Unmarshal(f.Data, &ok); err != nil {
		t.Fatal(err)
	}
	if ok.Mirrors == nil || len(*ok.Mirrors) != 2 || (*ok.Mirrors)[0] != "docker.1ms.run" {
		t.Fatalf("回执里应当带两个加速地址，实际 %+v", ok.Mirrors)
	}
}

// 开关关掉时一个字节都不发。
//
// 这里必须能区分「没配」与「配了空」：前者是 Hub 不管这件事（节点保留
// 自己的配置），后者是用户把那一格删了（节点要回到自己的配置）。
// 两者在 JSON 里都是缺少字段/空数组，所以回执用指针表达三态。
func TestHelloOKOmitsMirrorsWhenOff(t *testing.T) {
	srv, _, ts, _ := newTestHub(t)
	srv.cfg.Mirrors = func() *[]string { return nil }

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()

	f := sendHello(t, c, hubproto.Protocol)
	var ok hubproto.HelloOK
	if err := json.Unmarshal(f.Data, &ok); err != nil {
		t.Fatal(err)
	}
	if ok.Mirrors != nil {
		t.Fatalf("开关关着不该下发任何地址，实际 %+v", *ok.Mirrors)
	}
}

// 保存设置后，已连接的节点要当场收到新地址。
//
// 只靠下次重连生效是不够的：Agent 一旦连上就会一直连着，
// 用户改完设置看到「已保存」，节点那边可能几天后才知道。
func TestBroadcastConfigReachesConnectedNode(t *testing.T) {
	srv, _, ts, _ := newTestHub(t)
	srv.cfg.Mirrors = func() *[]string {
		m := []string{"new.mirror"}
		return &m
	}

	c, _, err := dialAgent(t, ts, "secret-token")
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	sendHello(t, c, hubproto.Protocol)

	srv.BroadcastConfig()

	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var f hubproto.Frame
	if err := c.ReadJSON(&f); err != nil {
		t.Fatalf("没收到配置帧: %v", err)
	}
	if f.Type != hubproto.TypeCmd || f.Action != hubproto.ActionConfig {
		t.Fatalf("应收到 config 指令，实际 type=%s action=%s", f.Type, f.Action)
	}
	var cmd hubproto.Cmd
	if err := json.Unmarshal(f.Data, &cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.Mirrors == nil || len(*cmd.Mirrors) != 1 || (*cmd.Mirrors)[0] != "new.mirror" {
		t.Fatalf("下发的地址不对：%+v", cmd.Mirrors)
	}
}

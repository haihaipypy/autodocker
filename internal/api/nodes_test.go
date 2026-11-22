package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/haihaipypy/autodocker/internal/hubserver"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/store"
)

// 用户填的地址决定了安装命令能不能用。这里只校验形态，不校验连通性 ——
// 但形态错了（少了协议头、带了端口没写协议）必须当场拦住，
// 否则生成出一条跑不起来的命令，用户只会以为「装完了但 Hub 里没有」。
func TestValidateHubURL(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"", false},
		{"   ", false},
		{"192.168.2.10:19999", false},       // 少了协议头
		{"hub.1day.vip", false},             // 同上
		{"http://192.168.2.10:19999", true}, // 局域网
		{"https://hub.1day.vip", true},      // 公网 + TLS
		{"https://hub.1day.vip/fnos", true}, // 反代在子路径下
		{"ftp://hub.1day.vip", false},       // 协议不对
		{"http://", false},                  // 没有主机
		{"https://hub.1day.vip?x=1", false}, // 带查询参数
		{"https://hub.1day.vip#a", false},   // 带锚点
		{"ws://hub.1day.vip", false},        // Agent 侧自己会换 ws，不归用户填
	}
	for _, c := range cases {
		got := validateHubURL(c.in)
		if c.ok && got != "" {
			t.Errorf("%q 应通过，却报错: %s", c.in, got)
		}
		if !c.ok && got == "" {
			t.Errorf("%q 应被拒绝，却通过了", c.in)
		}
	}
}

// 安装脚本是用户直接 `curl | sh` 的东西，出一点问题都是灾难性的：
// 引号没转好会把地址截断、少一个 set -eu 会把半截失败当成功。
func TestScriptTemplate(t *testing.T) {
	sh := scriptTemplate("http://192.168.2.10:19999", "haihaipypy/autodocker:latest")

	required := []string{
		"#!/bin/sh",                        // 必须是 POSIX sh，群晖/飞牛/OpenWrt 都能跑
		"set -eu",                          // 出错就停，别带着半截状态继续
		"--token=",                         // 令牌从参数读，脚本里不留明文
		"docker.sock:/var/run/docker.sock", // 不给 socket 就看不到任何容器
		"autodocker-agent-data",            // 数据卷要命名，升级时才能保住令牌
		"restart=unless-stopped",           // 开机自启靠它
		"AUTODOCKER_MODE=agent",            // 角色切换
		"http://192.168.2.10:19999",        // Hub 地址
		"haihaipypy/autodocker:latest",
	}
	for _, want := range required {
		if !strings.Contains(sh, want) {
			t.Errorf("脚本缺少关键片段 %q", want)
		}
	}

	// 脚本里不能出现「占位符没替换」的痕迹
	if strings.Contains(sh, "__HUB_URL__") || strings.Contains(sh, "%s") {
		t.Error("脚本里残留了未替换的占位符")
	}
	// 没有令牌时必须报错退出，不能静默装一个连不上的 Agent
	if !strings.Contains(sh, "缺少节点令牌") {
		t.Error("脚本缺少空令牌的守卫")
	}
}

// 地址里带单引号时不能把 shell 引号闭合掉，否则后面的内容会被当成命令执行。
//
// 判断方式不是查字符串——转义后的内容里照样含有 `; rm`，肉眼看着像没转，
// 实际它在单引号内部。真正可靠的是让 shell 自己解析一遍：引号没配对就报语法错。
func TestScriptTemplateIsValidShell(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("当前环境没有 sh，跳过语法校验")
	}

	for _, hub := range []string{
		"http://192.168.2.10:19999",
		"https://hub.example.com'; rm -rf /tmp/x; echo '",
		"https://hub.example.com/$(whoami)/`id`",
	} {
		script := scriptTemplate(hub, "haihaipypy/autodocker:latest")
		f := filepath.Join(t.TempDir(), "install.sh")
		if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
			t.Fatalf("写临时脚本失败: %v", err)
		}
		out, err := exec.Command(shPath, "-n", f).CombinedOutput()
		if err != nil {
			t.Fatalf("地址 %q 生成的脚本语法不通过: %v\n%s\n---\n%s", hub, err, out, script)
		}
	}
}

// 转义后的形式必须和 shQuote 的输出一致，防止哪天有人把 shQuote 摘掉。
func TestScriptTemplateUsesQuotedURL(t *testing.T) {
	hub := "https://hub.example.com"
	script := scriptTemplate(hub, "img:latest")
	if !strings.Contains(script, "HUB_URL='"+hub+"'") {
		t.Errorf("Hub 地址没有被单引号包裹:\n%s", script)
	}
}

// 远端更新接口的第一道关卡。
//
// 拦不住的后果比「接口报错」严重得多：指令一旦发到目标机，那边会真的
// 停掉容器再重建。所以「没有可用更新」「已被忽略」「容器编号对不上」
// 这些必须在这里就挡住，而不是指望 Agent 再判一次。
func TestNodeUpdateRejectsBadRequests(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	nodeID, err := st.CreateNode(&model.Node{Name: "测试机", HubURL: "http://h:1"}, hashNodeToken("tok"))
	if err != nil {
		t.Fatalf("建节点失败: %v", err)
	}
	if err := st.ReplaceNodeContainers(nodeID, []model.NodeContainer{
		{NodeID: nodeID, ContainerID: "c-upd", ContainerName: "web", ImageRef: "nginx:latest",
			Origin: model.OriginContainer, Status: model.StatusUpdatePending},
		{NodeID: nodeID, ContainerID: "c-ok", ContainerName: "db", ImageRef: "redis:7",
			Origin: model.OriginContainer, Status: model.StatusUpToDate},
		{NodeID: nodeID, ContainerID: "c-ign", ContainerName: "old", ImageRef: "x:1",
			Origin: model.OriginContainer, Status: model.StatusIgnored, Ignored: true},
	}); err != nil {
		t.Fatalf("写快照失败: %v", err)
	}

	// 节点从没连过，任何走到「下发」这一步的请求都会因为离线被拒 ——
	// 这正好让校验分支和下发分支各归各位。
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{st: st, hub: hubserver.New(hubserver.Config{}, st, logger)}

	cases := []struct {
		name string
		body string
		want int
		why  string
	}{
		{"缺容器编号", `{}`, http.StatusBadRequest, "容器编号"},
		{"容器不在这台机器上", `{"container_id":"nope"}`, http.StatusNotFound, "没有这个容器"},
		{"已是最新", `{"container_id":"c-ok"}`, http.StatusConflict, "没有可用的更新"},
		{"已忽略", `{"container_id":"c-ign"}`, http.StatusConflict, "已被忽略"},
		{"节点不在线", `{"container_id":"c-upd"}`, http.StatusConflict, "不在线"},
	}

	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/nodes/1/update", strings.NewReader(c.body))
		req.SetPathValue("id", strconv.FormatInt(nodeID, 10))
		rec := httptest.NewRecorder()
		s.handleNodeUpdate(rec, req)

		if rec.Code != c.want {
			t.Errorf("%s：期望 %d，实际 %d（%s）", c.name, c.want, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), c.why) {
			t.Errorf("%s：拒绝理由应包含 %q，实际 %s", c.name, c.why, rec.Body.String())
		}
	}
}

// 列表接口要把「此刻哪台在更新」一起带出去，前端靠这个画横幅。
func TestListNodesIncludesOps(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{st: st, hub: hubserver.New(hubserver.Config{}, st, logger)}

	req := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
	rec := httptest.NewRecorder()
	s.handleListNodes(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	var body struct {
		Nodes []model.Node                  `json:"nodes"`
		Ops   map[string]*hubserver.OpState `json:"ops"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应解析失败: %v（%s）", err, rec.Body.String())
	}
	if body.Nodes == nil {
		t.Error("没有节点时应返回空数组而不是 null")
	}
	if body.Ops == nil {
		t.Error("ops 应是空对象而不是 null —— 前端要拿它直接 Object.keys")
	}
}

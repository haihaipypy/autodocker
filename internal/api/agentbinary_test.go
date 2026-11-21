package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/haihaipypy/autodocker/internal/config"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/selfcheck"
	"github.com/haihaipypy/autodocker/internal/store"
)

func newTestServerWithNode(t *testing.T) (*Server, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	token := "0123456789abcdef0123456789abcdef"
	if _, err := st.CreateNode(&model.Node{Name: "测试机", HubURL: "http://h:1"}, hashNodeToken(token)); err != nil {
		t.Fatalf("建节点失败: %v", err)
	}
	return &Server{st: st}, token
}

// 没令牌或令牌不对时不能下发 —— 否则这个接口就是个免费的二进制 CDN。
func TestAgentBinaryRejectsBadToken(t *testing.T) {
	s, token := newTestServerWithNode(t)

	cases := []struct{ name, hdr, val string }{
		{"没有令牌", "", ""},
		{"令牌错误", "X-Agent-Token", "ffffffffffffffffffffffffffffffff"},
		{"空令牌", "Authorization", "Bearer "},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/agent/binary", nil)
		if c.hdr != "" {
			req.Header.Set(c.hdr, c.val)
		}
		rec := httptest.NewRecorder()
		s.handleAgentBinary(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: 应返回 401，实际 %d", c.name, rec.Code)
		}
	}

	// 反向确认：正确令牌不该被拦，免得上面三条通过是因为别的原因挂了
	req := httptest.NewRequest(http.MethodGet, "/api/agent/binary", nil)
	req.Header.Set("X-Agent-Token", token)
	rec := httptest.NewRecorder()
	s.handleAgentBinary(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Error("令牌正确却被拒绝")
	}
}

// 跨架构时必须明确拒绝并指向镜像方式，不能让脚本拿到一个跑不起来的文件。
func TestAgentBinaryRejectsOtherArch(t *testing.T) {
	s, token := newTestServerWithNode(t)

	oldOS := hostGOOS
	hostGOOS = "linux"
	t.Cleanup(func() { hostGOOS = oldOS })

	other := "arm64"
	if hostGOARCH == "arm64" {
		other = "amd64"
	}
	req := httptest.NewRequest(http.MethodGet, "/api/agent/binary?arch="+other, nil)
	req.Header.Set("X-Agent-Token", token)
	rec := httptest.NewRecorder()
	s.handleAgentBinary(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("应返回 409，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "--image") {
		t.Error("错误信息里要给出镜像方式的出路，否则用户只能卡在这")
	}
}

// 同架构、有令牌、Hub 跑在 Linux 上 → 必须真的把可执行文件发出去。
// 验证的是「发出去的不是空文件」而不是具体内容：不同平台编出来的格式不一样。
func TestAgentBinaryServesExecutable(t *testing.T) {
	s, token := newTestServerWithNode(t)

	oldOS := hostGOOS
	hostGOOS = "linux"
	t.Cleanup(func() { hostGOOS = oldOS })

	req := httptest.NewRequest(http.MethodGet, "/api/agent/binary?arch="+hostGOARCH, nil)
	req.Header.Set("X-Agent-Token", token)
	rec := httptest.NewRecorder()
	s.handleAgentBinary(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() < 1<<20 {
		t.Fatalf("下发的内容太小（%d 字节），不像一个 Go 二进制", rec.Body.Len())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type 应为 application/octet-stream，实际 %q", ct)
	}
	// 不许缓存：Hub 升级后重跑命令必须拿到新版本
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control 应为 no-store，实际 %q", cc)
	}
}

// 非 Linux 的 Hub（比如开发时的 Windows）不能发二进制 ——
// 发出去的是 .exe，脚本 chmod +x 之后照样跑不起来，只会让人查半天。
func TestAgentBinaryRejectsNonLinuxHub(t *testing.T) {
	s, token := newTestServerWithNode(t)

	oldOS := hostGOOS
	hostGOOS = "windows"
	t.Cleanup(func() { hostGOOS = oldOS })

	req := httptest.NewRequest(http.MethodGet, "/api/agent/binary", nil)
	req.Header.Set("X-Agent-Token", token)
	rec := httptest.NewRecorder()
	s.handleAgentBinary(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("应返回 400，实际 %d", rec.Code)
	}
}

// 安装脚本的主路径改成「从 Hub 下二进制」，这条链路的关键片段一个都不能少。
func TestScriptTemplateDownloadsBinary(t *testing.T) {
	sh := scriptTemplate("http://192.168.2.10:19999", "haihaipypy/autodocker:1.7.3")

	required := []string{
		"/api/agent/binary?arch=$ARCH", // 从 Hub 取二进制
		"X-Agent-Token: $TOKEN",        // 带令牌，接口不接受匿名下载
		`case "$(uname -m)" in`,        // 自己认架构
		"alpine:3.24",                  // 跑二进制的底
		"--entrypoint /app/autodocker", // 覆盖 alpine 的默认入口
		"/app/autodocker:ro",           // 二进制只读挂进容器
		"--image",                      // 跨架构的回退开关
		"autodocker.new",               // 先下临时文件，避免半个可执行文件
	}
	for _, want := range required {
		if !strings.Contains(sh, want) {
			t.Errorf("脚本缺少关键片段 %q", want)
		}
	}
	// 二进制没下成功时不能往下走，否则会用上一次留下的旧文件跑起来
	if !strings.Contains(sh, "exit 1") {
		t.Error("下载失败后没有退出")
	}
}

// 回退镜像的 tag 要跟 Hub 版本走，不能笼统给 latest：
// latest 指向哪一版永远不知道，这正是「装了跑成旧版」的根源。
func TestAgentImageFollowsVersion(t *testing.T) {
	s := &Server{cfg: &config.Config{}}

	old := selfcheck.Version
	t.Cleanup(func() { selfcheck.Version = old })

	selfcheck.Version = "1.7.3"
	if got := s.agentImage(); got != "haihaipypy/autodocker:1.7.3" {
		t.Errorf("应带上版本号，实际 %q", got)
	}

	// dev 构建没有可用的 tag，退回 latest 是唯一选择
	selfcheck.Version = "dev"
	if got := s.agentImage(); got != "haihaipypy/autodocker:latest" {
		t.Errorf("dev 构建应退回 latest，实际 %q", got)
	}
}

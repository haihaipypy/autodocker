package registry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseMirrors(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"空字符串", "", []string{}},
		{"纯空白", "   \n\t ", []string{}},
		{"单地址", "docker.m.daocloud.io", []string{"docker.m.daocloud.io"}},
		{"带协议与结尾斜杠", "https://docker.1ms.run/", []string{"docker.1ms.run"}},
		{"http 协议", "http://mirror.local:5000", []string{"mirror.local:5000"}},
		{"逗号分隔", "a.com,b.com", []string{"a.com", "b.com"}},
		{"逗号加空格", "a.com, b.com ,c.com", []string{"a.com", "b.com", "c.com"}},
		{"换行分隔", "a.com\nb.com", []string{"a.com", "b.com"}},
		{"分号分隔", "a.com;b.com", []string{"a.com", "b.com"}},
		{"去重且保序", "b.com,a.com,b.com", []string{"b.com", "a.com"}},
		{"丢弃误粘的路径", "https://a.com/v2/", []string{"a.com"}},
		{"大小写归一", "Docker.M.DaoCloud.IO", []string{"docker.m.daocloud.io"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseMirrors(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("ParseMirrors(%q) = %v, 期望 %v", c.in, got, c.want)
			}
		})
	}
}

// 内置默认地址必须能被解析成裸主机名，顺序也要原样保留。
//
// 它写成 "https://host/" 只是为了在设置页里看着眼熟，
// 万一哪天有人图省事把常量改成别的写法，这条能立刻发现问题。
func TestDefaultMirrorParses(t *testing.T) {
	got := ParseMirrors(DefaultMirror)
	want := []string{"docker.1ms.run", "docker.xuanyuan.me"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseMirrors(DefaultMirror) = %v, 期望 %v", got, want)
	}
	if len(got) != 2 {
		t.Fatalf("内置默认应恰好两个源，实际 %d 个", len(got))
	}
	// 最快的必须排第一：加速源按序回退，慢的排前面会让每轮扫描白等。
	if got[0] != "docker.1ms.run" {
		t.Fatalf("首位应为实测最快的 docker.1ms.run，实际 %q", got[0])
	}
}

func TestIsDockerHub(t *testing.T) {
	yes := []string{DefaultRegistryAPI, DefaultRegistry, "index.docker.io", "registry.hub.docker.com"}
	for _, h := range yes {
		if !IsDockerHub(h) {
			t.Fatalf("IsDockerHub(%q) 应为 true", h)
		}
	}
	no := []string{"ghcr.io", "quay.io", "registry.cn-hangzhou.aliyuncs.com", "192.168.1.10:5000"}
	for _, h := range no {
		if IsDockerHub(h) {
			t.Fatalf("IsDockerHub(%q) 应为 false", h)
		}
	}
}

// 加速源只该接管 Docker Hub。
//
// 这是曾经踩过的坑：把 ghcr.io 的请求也一起改发到公共加速站，
// 结果全站镜像都读不到——公共站压根不缓存 Docker Hub 以外的仓库。
func TestRoutesOnlyAcceleratesDockerHub(t *testing.T) {
	c := New(Options{Mirrors: []string{"m1.io", "m2.io"}})

	hub, err := ParseReference("nginx:latest")
	if err != nil {
		t.Fatal(err)
	}
	routes := c.routesFor(hub)
	if len(routes) != 3 {
		t.Fatalf("Docker Hub 应有 3 条路由（两个加速 + 直连），实际 %d", len(routes))
	}
	if routes[0].host != "m1.io" || !routes[0].mirror {
		t.Fatalf("首条应为加速源 m1.io，实际 %+v", routes[0])
	}
	if routes[1].host != "m2.io" || !routes[1].mirror {
		t.Fatalf("次条应为加速源 m2.io，实际 %+v", routes[1])
	}
	if routes[2].host != DefaultRegistryAPI || routes[2].mirror {
		t.Fatalf("最后一条必须是直连兜底，实际 %+v", routes[2])
	}

	ghcr, err := ParseReference("ghcr.io/freeok/sonovel:latest")
	if err != nil {
		t.Fatal(err)
	}
	routes = c.routesFor(ghcr)
	if len(routes) != 1 || routes[0].host != "ghcr.io" || routes[0].mirror {
		t.Fatalf("非 Docker Hub 必须直连，实际 %+v", routes)
	}
}

func TestRoutesWithoutMirrors(t *testing.T) {
	c := New(Options{})
	hub, _ := ParseReference("nginx")
	routes := c.routesFor(hub)
	if len(routes) != 1 || routes[0].host != DefaultRegistryAPI {
		t.Fatalf("未配加速时应只有直连一条，实际 %+v", routes)
	}
}

// 加速源失败时必须继续往下一个源走，最后还有直连兜底。
func TestManifestDigestFallsBack(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:deadbeef")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer good.Close()

	c := New(Options{
		Mirrors:  []string{hostOf(bad.URL), hostOf(good.URL)},
		Insecure: true,
	})
	ref, err := ParseReference("nginx:latest")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.ManifestDigest(context.Background(), ref)
	if err != nil {
		t.Fatalf("应当在第二个源成功，却报错: %v", err)
	}
	if got != "sha256:deadbeef" {
		t.Fatalf("digest = %q", got)
	}
}

// 全部失败时要把每个源的原因都带上，而不是只留最后一个。
//
// 这里只测聚合规则本身：真的发请求去验证「直连是否可达」并不可靠，
// 开发机上挂个代理就会让结果完全变样。
func TestSummarizeFailures(t *testing.T) {
	attempts := []string{"m1.io: 连接超时", DefaultRegistryAPI + ": 连接超时"}
	err := summarizeFailures(attempts, 0, 0)
	if err == nil {
		t.Fatal("应当报错")
	}
	if !strings.Contains(err.Error(), "所有镜像源均失败") {
		t.Fatalf("错误信息未说明是多源聚合: %v", err)
	}
	for _, a := range attempts {
		if !strings.Contains(err.Error(), a) {
			t.Fatalf("错误信息漏掉了 %q: %v", a, err)
		}
	}

	if got := summarizeFailures([]string{"m1.io: 镜像或 tag 不存在"}, 1, 0); got != ErrNotFound {
		t.Fatalf("全 404 应归为 ErrNotFound，实际 %v", got)
	}
	if got := summarizeFailures([]string{"m1.io: 要求登录认证（HTTP 401）"}, 0, 1); got != ErrUnauthorized {
		t.Fatalf("全 401 应归为 ErrUnauthorized，实际 %v", got)
	}
	// 一个源说不存在、另一个源超时，就不是「不存在」，而是「没查成」
	mixed := summarizeFailures([]string{"m1.io: 镜像或 tag 不存在", "m2.io: 连接超时"}, 1, 0)
	if mixed == ErrNotFound {
		t.Fatalf("混合失败不应归为 ErrNotFound: %v", mixed)
	}

	// 403 的理由各家不同，必须原样带出去：压成「需要登录凭据」会把人
	// 引去配账号，配完还是 403。这一条盯的是 1.10.4 之前的那句错话。
	denied := summarizeFailures([]string{"m1.io: 拒绝访问（HTTP 403）：免费节点当前繁忙，请稍后重试"}, 0, 0)
	if !strings.Contains(denied.Error(), "免费节点当前繁忙") {
		t.Fatalf("403 的原始说明被丢掉了: %v", denied)
	}
	if strings.Contains(denied.Error(), "登录凭据") {
		t.Fatalf("403 不该说成需要登录凭据: %v", denied)
	}
}

func TestFriendlyErr(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"dial tcp: lookup registry-1.docker.io: no such host", "域名解析失败"},
		{"dial tcp 1.2.3.4:443: i/o timeout", "连接超时"},
		{"dial tcp 127.0.0.1:443: connect: connection refused", "连接被拒绝"},
	}
	for _, c := range cases {
		got := friendlyErr(errString(c.in))
		if !strings.Contains(got, c.want) {
			t.Fatalf("friendlyErr(%q) = %q，期望包含 %q", c.in, got, c.want)
		}
	}
	if got := friendlyErr(ErrNotFound); !strings.Contains(got, "不存在") {
		t.Fatalf("哨兵错误应被翻译，实际 %q", got)
	}
}

// hostOf 把 httptest 的 URL 变成我们内部使用的裸主机名。
func hostOf(raw string) string {
	return strings.TrimPrefix(strings.TrimPrefix(raw, "http://"), "https://")
}

type errString string

func (e errString) Error() string { return string(e) }

// 403 的理由必须原样摘出来。
//
// 现场是这样的：docker.1ms.run 对 chaitin/* 的镜像层回 403 加一句
// 「欢迎联系我们获取帮助 QQ 群」，docker.xuanyuan.me 回 403 加
// 「免费节点当前繁忙，请稍后重试」。这句话是用户唯一能照着做的线索，
// 统一说成「需要登录凭据」会让人去配账号，配完还是 403。
func TestBodySnippet(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "registry 规范的 errors",
			body: `{"errors":[{"code":"DENIED","message":"免费节点当前繁忙，请稍后重试"}]}`,
			want: "免费节点当前繁忙",
		},
		{
			name: "顶层 message",
			body: `{"message":"需要付费通道"}`,
			want: "需要付费通道",
		},
		{
			name: "反代甩回来的 HTML",
			body: `<html><head><title>403</title></head><body><h1>403 Forbidden</h1><p>请求过于频繁</p></body></html>`,
			want: "请求过于频繁",
		},
	}
	for _, c := range cases {
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(c.body))}
		got := bodySnippet(resp)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s：应摘出 %q，实际 %q", c.name, c.want, got)
		}
	}

	// 超长要截断，而且不能把中文按字节切成半个字。
	long := strings.Repeat("免费节点当前繁忙", 40)
	got := bodySnippet(&http.Response{Body: io.NopCloser(strings.NewReader(long))})
	if !utf8.ValidString(got) {
		t.Errorf("截断后不是合法的 UTF-8：%q", got)
	}
	if n := len([]rune(got)); n > 121 {
		t.Errorf("截断到 120 字以内，实际 %d 字", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("截断应有省略号：%q", got)
	}
}

func TestDeniedErrorWording(t *testing.T) {
	err := &deniedError{reason: "免费节点当前繁忙，请稍后重试"}
	if !errors.Is(err, ErrDenied) {
		t.Fatal("应能被 errors.Is 认成 ErrDenied")
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Error("403 不该被当成 401：两者要走不同的提示")
	}
	if got := friendlyErr(err); !strings.Contains(got, "403") || !strings.Contains(got, "免费节点当前繁忙") {
		t.Errorf("提示要带状态码与原文，实际 %q", got)
	}
	if got := friendlyErr(err); strings.Contains(got, "登录") {
		t.Errorf("403 不该说成需要登录：%q", got)
	}
}

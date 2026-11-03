package selfcheck

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubGitHub 造一个假的 GitHub：/releases/latest 返回 302，/api 返回 JSON。
func stubGitHub(t *testing.T, tag, body string) *Checker {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		// 真实 GitHub 就是 302 到 /releases/tag/<tag>，版本号藏在 Location 尾巴上
		http.Redirect(w, r, "/releases/tag/"+tag, http.StatusFound)
	})
	mux.HandleFunc("/api/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"` + tag + `","body":"` + body + `"}`))
	})
	// 200 但没有 Location：模拟「响应形态不对」（被限流页顶掉之类）
	mux.HandleFunc("/nothing", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not a redirect"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := New(slog.New(slog.NewTextHandler(discard{}, nil)))
	c.latestURL = srv.URL + "/releases/latest"
	c.apiURL = srv.URL + "/api/releases/latest"
	return c
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// withVersion 临时改掉包级 Version。测试串行跑，改完还原即可。
func withVersion(t *testing.T, v string) {
	t.Helper()
	old := Version
	Version = v
	t.Cleanup(func() { Version = old })
}

// withMirrors 临时换掉内置的公共加速清单。
func withMirrors(t *testing.T, list ...string) {
	t.Helper()
	old := PublicMirrors
	PublicMirrors = list
	t.Cleanup(func() { PublicMirrors = old })
}

func TestCheckFindsNewVersion(t *testing.T) {
	withVersion(t, "1.4.1")
	c := stubGitHub(t, "v1.5.0", `## 新增\n- 自检更新\n**Full Changelog**: https://github.com/x/y/compare/a...b`)

	st := c.Check(context.Background(), true)

	if st.Error != "" {
		t.Fatalf("不该报错：%s", st.Error)
	}
	if !st.HasUpdate || st.Latest != "v1.5.0" {
		t.Fatalf("应判定有新版本，拿到 has_update=%v latest=%q", st.HasUpdate, st.Latest)
	}
	if st.ReleaseURL != RepoURL+"/releases/tag/v1.5.0" {
		t.Fatalf("Release 链接不对：%s", st.ReleaseURL)
	}
	if len(st.Notes) == 0 {
		t.Fatal("应该带回更新内容")
	}
	if len(st.Guides) != 2 {
		t.Fatalf("应该有两套升级步骤，拿到 %d 套", len(st.Guides))
	}
	if st.Guides[0].Steps[len(st.Guides[0].Steps)-1].Text == "" {
		t.Fatal("升级步骤不能是空的")
	}
}

// 本机版本比远端新（手测包 1.4.1 对线上 1.4.0）时不能报「有新版本」。
func TestCheckNoUpdateWhenLocalIsNewer(t *testing.T) {
	withVersion(t, "1.4.1")
	c := stubGitHub(t, "v1.4.0", "")

	st := c.Check(context.Background(), false)
	if st.HasUpdate {
		t.Fatalf("本地 %s 比远端 %s 新，不该提示更新", Version, st.Latest)
	}
}

// 没有新版本又不带 force 时不去打 API 取说明 —— 那是唯一按 IP 限流的接口。
func TestCheckSkipsNotesWithoutUpdate(t *testing.T) {
	withVersion(t, "1.5.0")

	hit := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/v1.5.0", http.StatusFound)
	})
	mux.HandleFunc("/api/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		hit++
		_, _ = w.Write([]byte(`{"tag_name":"v1.5.0","body":"x"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(nil)
	c.latestURL = srv.URL + "/releases/latest"
	c.apiURL = srv.URL + "/api/releases/latest"

	st := c.Check(context.Background(), false)
	if st.HasUpdate {
		t.Fatal("同版本不该提示更新")
	}
	if hit != 0 {
		t.Fatalf("没新版本时不该打 API，实际打了 %d 次", hit)
	}
}

// dev 构建不与远端比较，但版本号与升级步骤照样要给出来。
func TestCheckDevBuild(t *testing.T) {
	withVersion(t, "dev")
	c := stubGitHub(t, "v1.5.0", "")

	st := c.Check(context.Background(), false)
	if !st.DevBuild {
		t.Fatal("dev 构建应标记 dev_build")
	}
	if st.HasUpdate {
		t.Fatal("dev 构建不参与版本比较")
	}
	if st.Current != "dev" || len(st.Guides) != 2 || st.RepoURL == "" {
		t.Fatalf("dev 构建也要给出项目地址与升级步骤：%+v", st)
	}
}

// 302 拿不到版本时（被限流页顶掉之类）要能退回 API。
func TestLatestTagFallsBackToAPI(t *testing.T) {
	withMirrors(t) // 没有备用线路，只走直连
	c := stubGitHub(t, "v1.5.0", "")
	// 把重定向接口换成一个直接 200 的空响应，模拟「响应形态不对」
	c.latestURL = strings.Replace(c.latestURL, "/releases/latest", "/nothing", 1)

	h, err := c.latestTag(context.Background())
	if err != nil {
		t.Fatalf("兜底路径失败：%v", err)
	}
	if h.tag != "v1.5.0" {
		t.Fatalf("兜底拿到的 tag 不对：%s", h.tag)
	}
	if h.mirror != "" {
		t.Fatalf("这条是直连拿到的，mirror 应为空：%q", h.mirror)
	}
}

// 直连不通时自动换公共加速 —— 用户什么都不用填。
func TestFallsBackToPublicMirror(t *testing.T) {
	withVersion(t, "1.4.1")

	// 第一条备用线路不认路（真实服务偶尔会回 403），第二条正常。
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer bad.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.Redirect(w, r, "/releases/tag/v1.5.0", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<div class="markdown-body"><h2>新增</h2><ul><li>走加速也拿得到说明</li></ul></div>`))
	}))
	defer good.Close()

	withMirrors(t, bad.URL+"/", good.URL+"/")

	c := New(nil)
	// 指向必然连不上的地址，模拟国内直连 GitHub 超时。
	// 两个地址都要换，否则直连那条兜底会真去问 api.github.com。
	c.latestURL = "http://127.0.0.1:1/releases/latest"
	c.apiURL = "http://127.0.0.1:1/api/releases/latest"

	st := c.Check(context.Background(), true)
	if st.Error != "" {
		t.Fatalf("应当靠公共加速查通，实际报错：%s", st.Error)
	}
	if st.Latest != "v1.5.0" || !st.HasUpdate {
		t.Fatalf("应判定有新版本：latest=%q has_update=%v", st.Latest, st.HasUpdate)
	}
	if st.Via != good.URL+"/" {
		t.Fatalf("via 应记下生效的那条线路，实际 %q", st.Via)
	}
	if len(st.Notes) != 2 || st.Notes[0] != "## 新增" {
		t.Fatalf("更新内容应从前缀转发出来的 Release 页面读，实际 %v", st.Notes)
	}

	// 通了之后要记住这条线路，下次先试它，不再白等一次直连超时。
	routes := c.routes()
	if len(routes) == 0 || routes[0] != good.URL+"/" {
		t.Fatalf("下次应优先走上一次通的线路，实际 %v", routes)
	}
}

// 用户自己填了前缀就只用它，别自作主张换别的服务。
func TestExplicitMirrorOnly(t *testing.T) {
	withMirrors(t, "https://never-tried.example/")
	c := New(nil)
	c.Configure(Options{Mirror: "https://mine.example"})

	routes := c.routes()
	if len(routes) != 1 || routes[0] != "https://mine.example/" {
		t.Fatalf("指定了前缀就只该走它，实际 %v", routes)
	}
}

// 配了代理就不再叠加速 —— 代理本身已经绕过去了。
func TestProxyDisablesMirrors(t *testing.T) {
	withMirrors(t, "https://never-tried.example/")
	c := New(nil)
	c.Configure(Options{Proxy: "socks5h://127.0.0.1:1080"})

	routes := c.routes()
	if len(routes) != 1 || routes[0] != "" {
		t.Fatalf("配了代理只该走直连，实际 %v", routes)
	}
}

func TestTrimNotes(t *testing.T) {
	body := "## 新增\n\n- 自检更新\r\n---\n<!-- 注释 -->\n**Full Changelog**: https://github.com/a/b/compare/1...2\n## 修复\n- 空行要吃掉"
	got := trimNotes(body)

	want := []string{"## 新增", "- 自检更新", "## 修复", "- 空行要吃掉"}
	if len(got) != len(want) {
		t.Fatalf("行数不对：%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 行是 %q，期望 %q", i, got[i], want[i])
		}
	}
}

// API 被限流（403）时要退到 Atom 源，别让「更新内容」直接空着。
func TestNotesFallBackToAtom(t *testing.T) {
	withVersion(t, "1.4.1")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
	})
	mux.HandleFunc("/releases.atom", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <title>v1.5.0</title>
    <content type="html">&lt;h2&gt;新增&lt;/h2&gt;&lt;ul&gt;&lt;li&gt;&lt;strong&gt;自检更新&lt;/strong&gt;：每天查一次&lt;/li&gt;&lt;/ul&gt;</content>
  </entry>
</feed>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(nil)
	c.apiURL = srv.URL + "/api/releases/latest"
	c.atomURL = srv.URL + "/releases.atom"

	notes, err := c.releaseNotes(context.Background(), hit{tag: "v1.5.0"})
	if err != nil {
		t.Fatalf("兜底也失败了：%v", err)
	}
	want := []string{"## 新增", "- 自检更新：每天查一次"}
	if len(notes) != len(want) {
		t.Fatalf("解析结果不对：%v", notes)
	}
	for i := range want {
		if notes[i] != want[i] {
			t.Fatalf("第 %d 行是 %q，期望 %q", i, notes[i], want[i])
		}
	}
}

func TestHtmlNotes(t *testing.T) {
	in := `<p>开头一段</p><h2>新增</h2><ul><li>甲</li><li>乙</li></ul><p><strong>粗</strong>体</p>`
	got := trimNotes(htmlNotes(in))
	want := []string{"开头一段", "## 新增", "- 甲", "- 乙", "粗体"}
	if len(got) != len(want) {
		t.Fatalf("行数不对：%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 行是 %q，期望 %q", i, got[i], want[i])
		}
	}
}

// 走加速时，版本号与更新内容都得从「加速服务放行的那几个地址」拿到。
//
// 这条路的现实约束：加速服务只转发 github.com 下的页面，
// api.github.com 与 releases.atom 实测都是 404。所以版本号靠 302 的 Location，
// 更新内容靠读 Release 页面的 HTML —— 两者都不能去碰 API。
func TestMirrorModeReadsTagPage(t *testing.T) {
	withVersion(t, "1.4.1")

	var hit []string
	const tagPath = "/https://github.com/haihaipypy/autodocker/releases/tag/v1.5.0"
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = append(hit, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			// 加速服务会把 Location 改写成相对路径，前缀还留在里面 ——
			// 真实 gh-proxy 系服务就是这么回的。
			http.Redirect(w, r, tagPath, http.StatusFound)
		case r.URL.Path == tagPath:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><body>
<div class="markdown-body my-3">
  <h2 dir="auto">新增</h2>
  <ul dir="auto"><li>自检更新</li><li><strong>加速前缀</strong>：直连不通时用它</li></ul>
  <div class="highlight"><code>docker pull</code></div>
  <p>尾巴</p>
</div>
<div class="release-footer">这段不该进正文</div>
</body></html>`))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := New(nil)
	c.Configure(Options{Mirror: srv.URL})

	st := c.Check(context.Background(), true)
	if st.Error != "" {
		t.Fatalf("不该报错：%s", st.Error)
	}
	if st.Latest != "v1.5.0" || !st.HasUpdate {
		t.Fatalf("应判定有新版本：latest=%q has_update=%v", st.Latest, st.HasUpdate)
	}
	want := []string{"## 新增", "- 自检更新", "- 加速前缀：直连不通时用它", "docker pull", "尾巴"}
	if len(st.Notes) != len(want) {
		t.Fatalf("更新内容解析不对：%v", st.Notes)
	}
	for i := range want {
		if st.Notes[i] != want[i] {
			t.Fatalf("第 %d 行是 %q，期望 %q", i, st.Notes[i], want[i])
		}
	}
	// 所有请求都必须落在这台加速服务上：不能绕过它直接去问 GitHub，
	// 也不能去碰不转发的那两条（atoms）。API 是允许试的 ——
	// gh-proxy.com 那类服务恰好只转 API、不转网页。
	for _, p := range hit {
		if strings.Contains(p, "releases.atom") {
			t.Fatalf("前缀模式下不该去碰 Atom 源：%s", p)
		}
	}
}

// gh-proxy.com 这类加速服务只转发 API、明确拒绝网页：
// 一次 API 就该拿到版本号和正文，不该再去要页面。
func TestMirrorAPIGivesNotesInOneShot(t *testing.T) {
	withVersion(t, "1.4.1")

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if !strings.Contains(r.URL.Path, "api.github.com") {
			http.Error(w, "Web page content is not allowed", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v1.5.0","body":"## 新增\n- 只转发 API 的加速服务也能用"}`))
	}))
	defer srv.Close()

	c := New(nil)
	c.Configure(Options{Mirror: srv.URL})

	st := c.Check(context.Background(), true)
	if st.Error != "" {
		t.Fatalf("不该报错：%s", st.Error)
	}
	if st.Latest != "v1.5.0" || !st.HasUpdate || st.Via != srv.URL+"/" {
		t.Fatalf("结果不对：latest=%q has_update=%v via=%q", st.Latest, st.HasUpdate, st.Via)
	}
	if len(st.Notes) != 2 || st.Notes[0] != "## 新增" {
		t.Fatalf("正文该直接从 API 拿：%v", st.Notes)
	}
	for _, p := range paths {
		if strings.Contains(p, "/releases/tag/") {
			t.Fatalf("API 已经有正文了，不该再去抓页面：%v", paths)
		}
	}
}

// 前缀写法的容错：少尾斜杠、少 scheme 都要能拼出能用的地址。
func TestNormalizeMirror(t *testing.T) {
	cases := map[string]string{
		"":                          "",
		"   ":                       "",
		"https://github.1day.vip":   "https://github.1day.vip/",
		"https://github.1day.vip/":  "https://github.1day.vip/",
		"github.1day.vip":           "https://github.1day.vip/",
		" http://10.0.0.2:8080/gh ": "http://10.0.0.2:8080/gh/",
	}
	for in, want := range cases {
		if got := NormalizeMirror(in); got != want {
			t.Fatalf("NormalizeMirror(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestWithMirrorOnlyWrapsGitHub(t *testing.T) {
	m := "https://gh.example/"
	if got := withMirror(m, LatestURL); got != m+LatestURL {
		t.Fatalf("github.com 地址应该套前缀：%s", got)
	}
	if got := withMirror(m, APIURL); got != APIURL {
		t.Fatalf("api.github.com 不该套前缀：%s", got)
	}
	if got := withMirror("", LatestURL); got != LatestURL {
		t.Fatalf("没配前缀时应原样返回：%s", got)
	}
}

func TestHumanizeErr(t *testing.T) {
	msg := humanizeErr(errors.New(`Get "https://github.com/x/y/releases/latest": net/http: TLS handshake timeout`))
	if !strings.Contains(msg, "代理") || !strings.Contains(msg, "公共加速") {
		t.Fatalf("超时应该给出可操作的提示，实际：%s", msg)
	}
	// Windows 的 refused 不说 refused，说的是这句
	win := humanizeErr(errors.New(`dial tcp 127.0.0.1:9: connectex: No connection could be made because the target machine actively refused it.`))
	if !strings.Contains(win, "公共加速") {
		t.Fatalf("Windows 的拒连也该给出可操作的提示，实际：%s", win)
	}
	if got := humanizeErr(errors.New("something odd")); !strings.Contains(got, "something odd") {
		t.Fatalf("认不出的错误也要带上原文：%s", got)
	}
}

// 层级不配平（页面被截断）时至少要把第一段交出来，不能整个空手。
func TestExtractReleaseBodyUnbalanced(t *testing.T) {
	page := `<div class="markdown-body">正文<div>还没关`
	if got := extractReleaseBody(page); !strings.Contains(got, "正文") {
		t.Fatalf("截断页面也该拿到正文：%q", got)
	}
	if got := extractReleaseBody(`<html>没有正文</html>`); got != "" {
		t.Fatalf("找不到正文时应返回空串：%q", got)
	}
}

// 升级步骤里的版本号要跟着最新版本走，否则用户会对着旧版本号照做。
// 页面上的版本号不带 v（1.5.0），步骤里说的是「页面上会变成什么」，也要跟着去掉。
func TestGuidesUseLatestVersion(t *testing.T) {
	gs := guides("v9.9.9")
	last := gs[0].Steps[len(gs[0].Steps)-1].Text
	if !strings.Contains(last, "9.9.9") || strings.Contains(last, "v9.9.9") {
		t.Fatalf("飞牛那套步骤的版本号没去掉 v：%s", last)
	}
	if !strings.Contains(gs[1].Steps[len(gs[1].Steps)-1].Text, "9.9.9") {
		t.Fatalf("Docker 那套步骤没带上最新版本号：%s", gs[1].Steps[len(gs[1].Steps)-1].Text)
	}
	if gs[1].Key != FormDocker || !strings.Contains(gs[1].Steps[0].Cmd, "docker pull") {
		t.Fatal("Docker 那套步骤不对")
	}
}

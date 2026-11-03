// Package selfcheck 检查 AutoDocker 自己有没有新版本，并给出对应的升级步骤。
//
// 只做「告诉用户有新版本」，不下载、不替换任何东西：飞牛应用包的安装归应用中心管，
// Docker 镜像的更新归 compose 管。程序自己动手去改这两样，一旦失败，
// 用户手上就没有能把它救回来的手段了 —— 而这一切换来的只是省一次点击。
package selfcheck

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/haihaipypy/autodocker/internal/version"
)

// Version 当前二进制自己的版本号，由构建脚本注入：
//
//	go build -ldflags "-X github.com/haihaipypy/autodocker/internal/selfcheck.Version=1.5.0"
//
// 没注入时是 dev。dev 不参与版本比较 —— 本地 go run 出来的东西本来就比任何
// 已发布的版本都新，硬比只会让它天天说「有更新」。
var Version = "dev"

const (
	// RepoURL 项目主页，更新弹窗底部的跳转链接用它。
	RepoURL = "https://github.com/haihaipypy/autodocker"
	// LatestURL 永远指向最近一个正式 Release。
	LatestURL = RepoURL + "/releases/latest"
	// APIURL 只在需要「更新内容」时打，见 Check 里的说明。
	APIURL = "https://api.github.com/repos/haihaipypy/autodocker/releases/latest"
	// AtomURL 是取更新内容的兜底来源：不吃 API 配额、不要 token，
	// 代价是正文为 HTML 而不是 markdown。
	AtomURL = RepoURL + "/releases.atom"

	// 部署形态：决定默认展示哪一套升级步骤。
	FormFnos   = "fnos"
	FormDocker = "docker"
	FormOther  = "other"
)

// PublicMirrors 内置的公共加速前缀，直连不通时按顺序自动试。
//
// 全部遵守 gh-proxy 约定：把完整地址接在前缀后面，即
// https://gh-proxy.com/https://github.com/owner/repo/releases/latest
//
// 顺序按实测排的，而且这三家能吃的东西不一样 ——
// gh-proxy.com 只转发 api.github.com（网页回 403），另外两家相反：
// 只转发网页，API 回 403。所以 probe 对每条线路都是 API 与页面都试一遍，
// 谁通就用谁。用户不必填任何东西，填了只是覆盖这份清单。
var PublicMirrors = []string{
	"https://gh-proxy.com/",
	"https://ghfast.top/",
	"https://ghproxy.net/",
}

// 单次探测的超时。直连给得比加速短：国内直连失败基本是「TCP 建得起来、
// TLS 迟迟不握手」，等满只会让用户盯着转圈；加速服务远在境外但链路通常更快。
// 四条线路全挂时最坏 6+10×3 秒，接口那边留了 40 秒的余量。
const (
	directTimeout = 6 * time.Second
	mirrorTimeout = 10 * time.Second
)

// Step 升级步骤里的一步。Cmd 非空时前端会渲染成可复制的命令行。
type Step struct {
	Text string `json:"text"`
	Cmd  string `json:"cmd,omitempty"`
}

// Guide 一种部署形态的完整升级流程。
type Guide struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Hint  string `json:"hint"`
	Steps []Step `json:"steps"`
}

// State 一次自检的结果，原样给前端。
type State struct {
	Current    string    `json:"current"`
	Latest     string    `json:"latest"`
	HasUpdate  bool      `json:"has_update"`
	DevBuild   bool      `json:"dev_build"`
	CheckedAt  time.Time `json:"checked_at"`
	Notes      []string  `json:"notes"`
	RepoURL    string    `json:"repo_url"`
	ReleaseURL string    `json:"release_url"`
	DeployForm string    `json:"deploy_form"`
	// Via 这次是从哪条线路查到版本的：空串表示直连，
	// 否则是生效的加速前缀。界面上据此说一句「经公共加速查询」。
	Via    string  `json:"via"`
	Guides []Guide `json:"guides"`
	Error  string  `json:"error,omitempty"`
}

// Checker 自检器。状态在内存里，重启即重置 —— 每天一次的事情不值得落库。
type Checker struct {
	log  *slog.Logger
	form string
	now  func() time.Time

	// 下面这组是「怎么连 GitHub」的配置，Configure 会整体换掉。
	// 设置页保存后即时生效，所以要能热替换 —— 换掉之后下一次请求就走新的。
	client    *http.Client
	opt       Options
	latestURL string
	apiURL    string
	atomURL   string

	mu      sync.Mutex
	state   State
	lastRun time.Time
	// viaMirror 上次成功的线路，空串表示直连。
	// 一天一次的事情本可以不复用，但用户点「检查更新」时能省掉一次
	// 必然超时的直连，体验差别很明显。
	viaMirror string
}

// Options 决定去哪查、怎么连。默认全空 —— 程序自己会先直连、
// 不通再自动换公共加速，用户不需要知道这些。
type Options struct {
	// Proxy 代理地址，http:// / https:// / socks5:// / socks5h:// 都行。
	// 留空时沿用环境变量 HTTPS_PROXY（Go 标准库的默认行为）。
	// NAS 上本来跑着 Clash 的话填上最稳，填了就不再走公共加速。
	Proxy string
	// Mirror 手动指定加速前缀，形如 https://gh-proxy.com/。
	// 填了就只用它，不再自动挑；留空则走内置清单。
	Mirror string
}

// New 构造自检器。
func New(log *slog.Logger) *Checker {
	if log == nil {
		log = slog.Default()
	}
	c := &Checker{
		log:  log,
		form: DetectForm(),
		now:  time.Now,
		state: State{
			Current:    displayVersion(),
			DevBuild:   Version == "dev",
			RepoURL:    RepoURL,
			ReleaseURL: LatestURL,
		},
	}
	c.Configure(Options{})
	return c
}

// Configure 换一套 GitHub 连接方式。传空值就是直连。
//
// 允许随时调用：用户在设置页填完代理点保存，下一次「检查更新」就该走代理，
// 而不是等重启。所以这里整体替换 client 与三个地址，不做增量修改。
func (c *Checker) Configure(o Options) {
	o.Proxy = strings.TrimSpace(o.Proxy)
	o.Mirror = NormalizeMirror(o.Mirror)

	// Clone 一份默认 Transport：它自带的环境变量代理、连接复用、
	// 各项超时都是调过的，没必要从零手搓。
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if o.Proxy != "" {
		pu, err := url.Parse(o.Proxy)
		if err != nil || pu.Host == "" {
			c.log.Warn("代理地址看不懂，按直连处理", "proxy", o.Proxy)
			o.Proxy = ""
		} else {
			tr.Proxy = http.ProxyURL(pu)
		}
	}

	c.mu.Lock()
	c.opt = o
	c.client = &http.Client{Timeout: 20 * time.Second, Transport: tr}
	c.latestURL = LatestURL
	c.apiURL = APIURL
	c.atomURL = AtomURL
	// 换了连接方式，上一条线路的结论就作废了：可能刚才只能走加速，
	// 配了代理之后直连也通。
	c.viaMirror = ""
	c.mu.Unlock()
}

// snapshot 取当前生效的连接配置。
//
// client 与几个地址会被 Configure 整体换掉，读写必须过锁 ——
// 否则设置页保存的瞬间正好有一次自检在跑，就会读到半新半旧的组合。
func (c *Checker) snapshot() (*http.Client, Options, string, string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client, c.opt, c.latestURL, c.apiURL, c.atomURL
}

// routes 本次要依次尝试的线路。空串代表直连，其余是加速前缀。
func (c *Checker) routes() []string {
	c.mu.Lock()
	opt, last := c.opt, c.viaMirror
	c.mu.Unlock()

	// 用户明确指定了前缀：只用它。他要的是「我这里就得走这个」，
	// 替他换别的服务反而更难解释。
	if opt.Mirror != "" {
		return []string{opt.Mirror}
	}
	// 配了代理就只走直连 —— 代理本身就是绕行方案，再叠一层加速只会更慢。
	if opt.Proxy != "" {
		return []string{""}
	}

	out := make([]string, 0, len(PublicMirrors)+1)
	// 上次靠某个前缀才通的，这次先试它：省掉一次必然超时的直连。
	// 但它也可能只是临时抽风，所以后面照样排着直连与其余前缀。
	if last != "" {
		out = append(out, last)
	}
	out = append(out, "")
	for _, m := range PublicMirrors {
		if m != last {
			out = append(out, m)
		}
	}
	return out
}

// rememberRoute 记下这条线路能用，下次先试它。
func (c *Checker) rememberRoute(mirror string) {
	c.mu.Lock()
	c.viaMirror = mirror
	c.mu.Unlock()
}

// routeName 给日志与页面看的线路名。
func routeName(mirror string) string {
	if mirror == "" {
		return "直连"
	}
	return "加速 " + strings.TrimSuffix(strings.TrimPrefix(mirror, "https://"), "/")
}

// NormalizeMirror 把加速前缀整成能直接拼 URL 的形状。
// 用户十有八九会漏尾斜杠，或者只填个域名 —— 这里都兜住。
// 导出是为了保存设置时先把值规整好再落库，页面上看到的就是真正生效的那个。
func NormalizeMirror(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	if !strings.HasSuffix(s, "/") {
		s += "/"
	}
	return s
}

// withMirror 给 github.com 的地址套上加速前缀。
//
// 写法沿用 gh-proxy 系约定：把完整 URL 直接接在前缀后面 ——
// https://gh.example/https://github.com/owner/repo/releases/latest
//
// 只挡两条不该套的：api.github.com 与 releases.atom 在多数加速服务的
// 白名单之外，套上去实测只得到 404 —— 那两条路见 releaseNotes 的说明。
func withMirror(mirror, raw string) string {
	if mirror == "" || !strings.HasPrefix(raw, "http") {
		return raw
	}
	if strings.Contains(raw, "api.github.com") || strings.Contains(raw, "releases.atom") {
		return raw
	}
	return mirror + raw
}

// displayVersion 页面与接口里展示的版本号。
//
// 飞牛 manifest 写的是 1.4.1，Docker 那边传进来的是 tag v1.5.0，
// 同一个东西不该在页面上出现两种写法。
func displayVersion() string { return strings.TrimPrefix(Version, "v") }

// DetectForm 判断自己是装成飞牛应用包还是 Docker 镜像。
//
// 优先认 TRIM_*：那两个变量只有飞牛的应用运行时才会注入。
// 反过来 Docker 容器里也可能存在同名目录，先判飞牛不会误伤。
func DetectForm() string {
	if os.Getenv("TRIM_PKGVAR") != "" || os.Getenv("TRIM_APPDEST") != "" {
		return FormFnos
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return FormDocker
	}
	return FormOther
}

// State 返回上一次检查的结果。没查过时只有版本号与升级步骤。
func (c *Checker) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fillLocked(c.state)
}

// fillLocked 补齐那些「任何时候都能算出来」的字段。
//
// 升级步骤与链接不依赖网络结果，所以没查到版本时也要给出来 ——
// 前端随时可能打开这个弹窗，不能一半是空的。
func (c *Checker) fillLocked(st State) State {
	if st.Current == "" {
		st.Current = displayVersion()
	}
	if st.RepoURL == "" {
		st.RepoURL = RepoURL
	}
	if st.ReleaseURL == "" {
		st.ReleaseURL = LatestURL
	}
	if st.DeployForm == "" {
		st.DeployForm = c.form
	}
	st.Guides = guides(st.Latest)
	return st
}

// Check 真正去查一次。force 为真时忽略最小间隔（用户点了「检查更新」）。
//
// 10 秒内重复调用直接返回上次结果：设置页的「检查更新」被连点时，
// 没必要对 GitHub 连打好几次。
func (c *Checker) Check(ctx context.Context, force bool) State {
	c.mu.Lock()
	tooSoon := !force && !c.lastRun.IsZero() && c.now().Sub(c.lastRun) < 10*time.Second
	c.mu.Unlock()
	if tooSoon {
		return c.State()
	}

	st := State{
		Current:   displayVersion(),
		DevBuild:  Version == "dev",
		CheckedAt: c.now(),
		RepoURL:   RepoURL,
	}

	h, err := c.latestTag(ctx)
	if err != nil {
		c.log.Info("自检失败", "err", err)
		// humanizeErr 给的是一整句给用户看的话，别再自己拼前缀
		st.Error = humanizeErr(err)
		return c.commit(st)
	}
	tag := h.tag
	st.Latest = tag
	st.Via = h.mirror
	st.ReleaseURL = RepoURL + "/releases/tag/" + tag

	// 版本号不是 x.y.z（dev、sha、带分支名）时无从比较，索性不比较。
	// 宁可不说，也别对着开发构建天天喊「有新版本」。
	if version.IsVersionTag(Version) && version.IsVersionTag(tag) {
		st.HasUpdate = version.Compare(tag, Version) > 0
	}

	// 「更新内容」要打 api.github.com，而它按 IP 限流（未认证 60 次/小时，
	// 家里 NAT 出口还常被别人一起用掉）。所以只在真发现新版本、
	// 或者用户主动点检查时才去取 —— 平时那次只走不计配额的重定向。
	if st.HasUpdate || force {
		notes, nerr := c.releaseNotes(ctx, h)
		if nerr != nil {
			// 取不到说明只影响「看不到更新内容」，版本号已经拿到了，
			// 不该因此把整个自检判成失败。
			c.log.Info("取 Release 说明失败，本次只给版本号", "err", nerr)
		} else {
			st.Notes = notes
		}
	}

	return c.commit(st)
}

// commit 补全派生字段并记下这次结果。
func (c *Checker) commit(st State) State {
	c.mu.Lock()
	out := c.fillLocked(st)
	c.state = out
	c.lastRun = out.CheckedAt
	c.mu.Unlock()
	return out
}

// hit 一条线路查到的结果。
type hit struct {
	tag  string
	body string // 走 API 时顺带拿到的 markdown 正文；页面线路为空
	// mirror 这次生效的线路，空串为直连。后续取更新内容要沿用同一条。
	mirror string
}

// latestTag 依次试每条线路，第一条通的就用它。
func (c *Checker) latestTag(ctx context.Context) (hit, error) {
	var firstErr error
	for _, r := range c.routes() {
		h, err := c.probe(ctx, r)
		if err == nil {
			c.rememberRoute(r)
			return h, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		// 外层已经取消（用户关页面、程序退出），别再一条条试下去。
		if err := ctx.Err(); err != nil {
			return hit{}, fmt.Errorf("所有线路都没能在超时内响应：%w", err)
		}
		c.log.Info("这条线路没通，换下一条", "route", routeName(r), "err", err)
	}
	if firstErr == nil {
		firstErr = errors.New("没有可用的线路")
	}
	return hit{}, firstErr
}

// probe 用一条线路探一次。
//
// 有两种查法，通不通取决于服务，所以两条都要试：
//   - api.github.com/repos/…/releases/latest：一次给全 tag_name 与正文，
//     但直连按 IP 限流（403），只转发网页的加速服务也不认它；
//   - github.com/…/releases/latest 的 302：Location 尾巴就是版本号，不吃配额，
//     但 gh-proxy.com 这类「只做资源下载」的服务明确拒绝网页。
//
// 配了加速前缀时先试 API（顺带拿到正文，省一次请求），失败再退页面；
// 直连反过来，先读不计配额的跳转，失败才打 API。
func (c *Checker) probe(ctx context.Context, mirror string) (hit, error) {
	if mirror != "" {
		if h, err := c.probeAPI(ctx, mirror); err == nil {
			return h, nil
		}
	}

	h, err := c.probeRedirect(ctx, mirror)
	if err == nil {
		return h, nil
	}
	if mirror == "" {
		// 直连读页面不通时还能试 API；走加速时则在上面已经试过了
		if h2, err2 := c.probeAPI(ctx, ""); err2 == nil {
			return h2, nil
		}
	}
	return hit{}, err
}

// probeAPI 走 API 查版本号，顺带把正文带回来。
func (c *Checker) probeAPI(ctx context.Context, mirror string) (hit, error) {
	_, _, _, apiURL, _ := c.snapshot()
	u := apiURL
	if mirror != "" {
		u = mirror + apiURL
	}

	actx, cancel := context.WithTimeout(ctx, routeTimeout(mirror))
	defer cancel()

	var out struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
	}
	if err := c.getJSON(actx, u, &out); err != nil {
		return hit{}, err
	}
	if out.TagName == "" {
		return hit{}, fmt.Errorf("%s 没返回 tag_name", routeName(mirror))
	}
	return hit{tag: out.TagName, body: out.Body, mirror: mirror}, nil
}

// probeRedirect 读 /releases/latest 的 302 跳转地址拿版本号。
//
// 版本号就在 Location 尾巴上（.../releases/tag/v1.4.0），这个请求不计入 API 配额。
// 家里出口 IP 是 NAT 共享的，走 API 经常一上来就 403 rate limit。
func (c *Checker) probeRedirect(ctx context.Context, mirror string) (hit, error) {
	client, _, base, _, _ := c.snapshot()
	u := withMirror(mirror, base)

	actx, cancel := context.WithTimeout(ctx, routeTimeout(mirror))
	defer cancel()

	req, err := http.NewRequestWithContext(actx, http.MethodGet, u, nil)
	if err != nil {
		return hit{}, err
	}
	req.Header.Set("User-Agent", "AutoDocker/"+Version)

	// 复制一份 client 再关掉跳转跟随：原 client 还要留给别的请求用。
	noFollow := *client
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		return hit{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	// 加速服务不认路时常常回 403/404 而不是拒绝连接，
	// 得当成「这条线路不行」，好让上层换下一条。
	if resp.StatusCode >= 400 {
		return hit{}, fmt.Errorf("%s 返回 HTTP %d", routeName(mirror), resp.StatusCode)
	}

	loc := resp.Header.Get("Location")
	if loc == "" {
		return hit{}, fmt.Errorf("%s 没给跳转（HTTP %d）", routeName(mirror), resp.StatusCode)
	}
	u2, err := url.Parse(loc)
	if err != nil {
		return hit{}, err
	}
	// 走加速前缀时 Location 是相对路径，形如
	// /https://github.com/.../releases/tag/v1.4.0 —— 前缀本身在 Path 里，
	// 但版本号永远在最后一段，所以取 Base 就够了。
	tag := path.Base(u2.Path)
	if tag == "" || tag == "latest" || tag == "." || tag == "/" {
		return hit{}, fmt.Errorf("跳转地址里没有版本号：%s", loc)
	}
	return hit{tag: tag, mirror: mirror}, nil
}

// routeTimeout 一条线路给多久。
func routeTimeout(mirror string) time.Duration {
	if mirror == "" {
		return directTimeout
	}
	return mirrorTimeout
}

// releaseNotes 取 Release 正文，按行拆成前端好排版的条目。
//
// 查版本那次顺带拿到正文就直接用；否则按线路去补：
//   - 走加速：读 Release 页面。（api.github.com 不是每家都转发，
//     只转发网页的那几家会回 403，别指望）
//   - 直连：先打 API（markdown 原文，排版最贴原样），失败退 Atom 源。
//     API 按 IP 限流，取不到只影响「看不到更新内容」，不影响版本号。
func (c *Checker) releaseNotes(ctx context.Context, h hit) ([]string, error) {
	if notes := trimNotes(h.body); len(notes) > 0 {
		return notes, nil
	}
	if h.mirror != "" {
		return c.notesFromTagPage(ctx, h.tag, h.mirror)
	}

	_, _, _, apiURL, _ := c.snapshot()
	var out struct {
		Body string `json:"body"`
	}
	if err := c.getJSON(ctx, apiURL, &out); err != nil {
		c.log.Info("API 取 Release 说明失败，改用 Atom 源", "err", err)
	} else if notes := trimNotes(out.Body); len(notes) > 0 {
		return notes, nil
	}
	return c.notesFromAtom(ctx, h.tag)
}

// notesFromTagPage 走加速前缀读 Release 页面，从 HTML 里抠出正文。
//
// 这是用加速服务时唯一拿得到内容的路：这类服务通常只转发
// github.com 下的页面，api.github.com 与 releases.atom 实测都是 404。
func (c *Checker) notesFromTagPage(ctx context.Context, tag, mirror string) ([]string, error) {
	if tag == "" {
		return nil, errors.New("没有版本号，拼不出 Release 页面地址")
	}
	client, _, base, _, _ := c.snapshot()
	u := withMirror(mirror, strings.Replace(base, "/releases/latest", "/releases/tag/"+tag, 1))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "AutoDocker/"+Version)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Release 页面返回 HTTP %d", resp.StatusCode)
	}
	// Release 页面带着整站导航，一百多 KB，正文只占其中一小块。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	body := extractReleaseBody(string(raw))
	if body == "" {
		return nil, errors.New("Release 页面里没找到更新说明")
	}
	return trimNotes(htmlNotes(body)), nil
}

// extractReleaseBody 从 Release 页面里抠出正文那一段。
//
// 正文包在 <div class="markdown-body …"> 里，而代码块、引用块还会在里面
// 再套一层 div —— 所以不能见第一个 </div> 就收手，得数层级配对。
func extractReleaseBody(page string) string {
	i := strings.Index(page, "markdown-body")
	if i < 0 {
		return ""
	}
	gt := strings.IndexByte(page[i:], '>')
	if gt < 0 {
		return ""
	}
	start := i + gt + 1

	depth, p := 1, start
	for p < len(page) && depth > 0 {
		open := strings.Index(page[p:], "<div")
		shut := strings.Index(page[p:], "</div>")
		if shut < 0 {
			break
		}
		if open >= 0 && open < shut {
			depth++
			p += open + len("<div")
			continue
		}
		depth--
		if depth == 0 {
			return page[start : p+shut]
		}
		p += shut + len("</div>")
	}
	// 层级没配平（页面被截断之类）时至少把剩下的交出去，总比空手强。
	if end := strings.Index(page[start:], "</div>"); end >= 0 {
		return page[start : start+end]
	}
	return page[start:]
}

// notesFromAtom 从 releases.atom 里挑出对应版本那一条。
func (c *Checker) notesFromAtom(ctx context.Context, tag string) ([]string, error) {
	client, _, _, _, atomURL := c.snapshot()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, atomURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "AutoDocker/"+Version)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var feed struct {
		Entries []struct {
			Title   string `xml:"title"`
			Content string `xml:"content"`
		} `xml:"entry"`
	}
	if err := xml.Unmarshal(raw, &feed); err != nil {
		return nil, err
	}
	if len(feed.Entries) == 0 {
		return nil, errors.New("Atom 源里没有条目")
	}
	pick := feed.Entries[0]
	for _, e := range feed.Entries {
		if strings.TrimSpace(e.Title) == tag {
			pick = e
			break
		}
	}
	return trimNotes(htmlNotes(pick.Content)), nil
}

// htmlNotes 把 Atom 里的 HTML 正文转成 trimNotes 认的那套「## 小标题 / - 列表」。
//
// Atom 里的 content 是转义过的 HTML，encoding/xml 读出来时已经还原成标签了，
// 所以这里直接按标签转格式、再把剩下的标签剥掉。
func htmlNotes(s string) string {
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	s = strings.ReplaceAll(s, "<br />", "\n")
	s = strings.ReplaceAll(s, "</p>", "\n")
	// 标签常带属性（<h2 dir="auto">、<li class="task-list-item">），
	// 所以按前缀匹配，不能写成精确的 "<h2>" —— 否则 GitHub 页面里
	// 一条都命中不了，正文会整段粘成一行。
	s = headingOpenRe.ReplaceAllString(s, "\n## ")
	s = headingCloseRe.ReplaceAllString(s, "\n")
	s = listItemOpenRe.ReplaceAllString(s, "\n- ")
	s = listItemClose.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "</ul>", "\n")
	s = strings.ReplaceAll(s, "</ol>", "\n")
	return tagStripper.ReplaceAllString(s, "")
}

var (
	headingOpenRe  = regexp.MustCompile(`(?i)<h[1-6][^>]*>`)
	headingCloseRe = regexp.MustCompile(`(?i)</h[1-6]>`)
	listItemOpenRe = regexp.MustCompile(`(?i)<li[^>]*>`)
	listItemClose  = regexp.MustCompile(`(?i)</li>`)
	tagStripper    = regexp.MustCompile(`<[^>]*>`)
)

// humanizeErr 把网络错误翻成「下一步该做什么」。
//
// 用户看到的是这一句，不是原始错误 —— TLS handshake timeout 这种
// 放在界面上等于什么都没说，而九成的成因就是国内连不上 GitHub。
func humanizeErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "proxyconnect"):
		// 先判这个：代理连不上时错误里常常也带着 connection refused，
		// 落到下面那条就变成「让你去填代理」，而用户明明已经填了。
		return "代理连不上，检查设置页里填的代理地址和端口。"
	case strings.Contains(s, "no such host"):
		return "解析不了 github.com，先看看这台机器的 DNS。"
	case strings.Contains(s, "certificate"):
		return "TLS 证书校验失败，可能被中间设备劫持了，换一条网络或代理再试。"
	case strings.Contains(s, "TLS handshake timeout"),
		strings.Contains(s, "i/o timeout"),
		strings.Contains(s, "context deadline exceeded"),
		strings.Contains(s, "connection reset"),
		strings.Contains(s, "connection refused"),
		// Windows 不说 connection refused，它说这句
		strings.Contains(s, "actively refused"),
		strings.Contains(s, "No connection could be made"),
		strings.Contains(s, "connectex"),
		// 加速服务不认路时回的状态码
		strings.Contains(s, "HTTP 403"),
		strings.Contains(s, "HTTP 404"),
		strings.Contains(s, "没给跳转"):
		return "查不到最新版本：直连和公共加速都没连上 GitHub。" +
			"设置页最下面「GitHub 访问」里填一个代理再试 —— NAS 上跑着 Clash 就填它的端口。"
	}
	return "查不到最新版本：" + s
}

func (c *Checker) getJSON(ctx context.Context, u string, dst any) error {
	client, _, _, _, _ := c.snapshot()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "AutoDocker/"+Version)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, dst)
}

// trimNotes 把 Release 正文切成一行一条，去掉对用户没意义的东西。
//
// 正文是自己按「新增 / 改进 / 修复」写的 markdown，标题行与小标题都保留，
// 前端会按 `#` / `-` 前缀分别渲染成小标题与列表项。
func trimNotes(body string) []string {
	out := make([]string, 0, 24)
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || s == "---" || s == "***" || s == "___" {
			continue
		}
		if strings.HasPrefix(s, "<!--") {
			continue
		}
		// GitHub 自动追加的「**Full Changelog**: https://…」对比链接。
		// 弹窗底部本来就给了项目地址和 Release 页，再塞一条超长链接只会把版面撑乱。
		if strings.Contains(strings.ToLower(s), "full changelog") {
			continue
		}
		out = append(out, s)
		// 再长也没人往下翻，40 行足够覆盖一次发版的说明。
		if len(out) >= 40 {
			break
		}
	}
	return out
}

// guides 生成两套升级步骤。latest 为空时用占位文案。
func guides(latest string) []Guide {
	// tag 带 v 前缀（v1.5.0），页面上的版本号不带（1.5.0）。
	// 步骤里说的是「页面上会变成什么」，所以这里跟着去掉。
	to := strings.TrimPrefix(latest, "v")
	if to == "" {
		// 还没查到版本号就把句子写成通的：「变成新的版本号就完成了」
		to = "新的版本号"
	}
	return []Guide{
		{
			Key:   FormFnos,
			Title: "飞牛应用包",
			Hint:  "装的是 .fpk，走应用中心升级。",
			Steps: []Step{
				{Text: "在 Release 页下载 Autodocker.fpk。x86 机型直接可用；ARM 机型需要在仓库目录执行 ./build-fpk.sh arm64 自己打一个。"},
				{Text: "飞牛桌面 → 应用中心 → 右上角「手动安装」，选中下载好的 Autodocker.fpk；也可以在 NAS 终端直接装：", Cmd: "appcenter-cli install-fpk Autodocker.fpk"},
				{Text: "安装完成后应用会自动重启。刷新本页，设置页最下面「关于」里的版本号变成 " + to + " 就完成了。"},
			},
		},
		{
			Key:   FormDocker,
			Title: "Docker 镜像",
			Hint:  "跑的是容器，拉新镜像后重建容器。",
			Steps: []Step{
				{Text: "拉取新镜像。Docker Hub 拉不动就换 GHCR，两边内容一致：", Cmd: "docker pull haihaipypy/autodocker:latest\n# 备选\ndocker pull ghcr.io/haihaipypy/autodocker:latest"},
				{Text: "用 compose 部署的，在 compose 文件所在目录执行：", Cmd: "docker compose pull && docker compose up -d"},
				{Text: "用 docker run 起的，删掉旧容器再按原参数重跑（数据目录换成你自己的）：", Cmd: "docker rm -f autodocker\ndocker run -d --name autodocker --restart unless-stopped \\\n  -p 17777:17777 \\\n  -v /var/run/docker.sock:/var/run/docker.sock \\\n  -v \"$PWD/data:/data\" \\\n  -e TZ=Asia/Shanghai \\\n  -e SELF_CONTAINER=autodocker \\\n  haihaipypy/autodocker:latest"},
				{Text: "刷新本页，设置页最下面「关于」里的版本号会变成 " + to + "。"},
			},
		},
	}
}

// Loop 后台定期检查。enabled 每轮问一次，用户在设置里关掉就彻底不出网。
func (c *Checker) Loop(ctx context.Context, enabled func() bool) {
	// 启动后稍等一下：这时扫描、通知长连接、Docker 连接都在抢资源，
	// 让一个网络请求排在它们后面几秒，不影响任何人的使用感受。
	// 但也别等太久 —— 用户装完就打开页面，那时应该已经有结果可看。
	wait := time.NewTimer(8 * time.Second)
	defer wait.Stop()
	select {
	case <-ctx.Done():
		return
	case <-wait.C:
		if enabled() {
			c.Check(ctx, false)
		}
	}

	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if !enabled() {
				continue
			}
			if !c.dueForCheck(24 * time.Hour) {
				continue
			}
			c.Check(ctx, false)
		}
	}
}

func (c *Checker) dueForCheck(every time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastRun.IsZero() || c.now().Sub(c.lastRun) >= every
}

package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// acceptManifest 列出我们愿意接受的 manifest 媒体类型。
// 必须同时包含 manifest list / OCI index，否则多架构镜像会退化成
// 单个平台 manifest，digest 与本地记录对不上。
const acceptManifest = "application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.v1+prettyjws"

// 哨兵错误，便于调用方区分「无更新」与「查询失败」。
var (
	ErrNotFound     = errors.New("注册表中不存在该 tag")
	ErrUnauthorized = errors.New("注册表要求登录认证")
	// ErrDenied 源明确拒绝这次请求（HTTP 403）。
	//
	// 与 ErrUnauthorized 分开：401 是「请先登录」，403 大多是「这个源不给你用」
	// —— 公共加速站对某些仓库只开放 tag 列表，镜像层要加群、充值或稍后再试，
	// 回的就是 403 加一句中文说明。把两者都说成「需要登录凭据」，
	// 用户去配完账号回来还是 403。
	ErrDenied = errors.New("镜像源拒绝访问")
)

// deniedError 403 的原始说明。响应体里那句话往往已经把原因写清楚了
// （「免费节点当前繁忙，请稍后重试」），比任何翻译都准确，原样带出去。
type deniedError struct{ reason string }

func (e *deniedError) Error() string {
	if e.reason == "" {
		return "拒绝访问（HTTP 403）"
	}
	return "拒绝访问（HTTP 403）：" + e.reason
}

// Is 让 errors.Is(err, ErrDenied) 成立，同时保留具体原因。
func (e *deniedError) Is(target error) bool { return target == ErrDenied }

// Options 客户端行为配置。
type Options struct {
	// Mirrors Docker Hub 的加速主机列表，按书写顺序回退。
	//
	// 只作用于 Docker Hub：ghcr.io / quay.io / 私有仓库一律直连。
	// 直连永远排在最后，所以列表为空时的行为与没有这个功能时完全一致。
	Mirrors []string

	Insecure  bool              // 允许 http 明文注册表
	Auth      map[string]string // host -> "user:pass" 或纯 token
	Timeout   time.Duration
	UserAgent string
}

// Client 一个并发安全的注册表客户端。
type Client struct {
	hc  *http.Client
	opt Options

	mu     sync.Mutex
	tokens map[string]tokenEntry
}

type tokenEntry struct {
	token   string
	expires time.Time
}

// New 构造注册表客户端。
func New(opt Options) *Client {
	opt = withDefaults(opt)
	return &Client{
		hc: &http.Client{
			Timeout: opt.Timeout,
			Transport: &http.Transport{
				// 关闭透明解压，保证兜底计算摘要时拿到的是注册表返回的原始字节
				DisableCompression:    true,
				MaxIdleConnsPerHost:   8,
				IdleConnTimeout:       60 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 20 * time.Second,
			},
		},
		opt:    opt,
		tokens: map[string]tokenEntry{},
	}
}

func withDefaults(opt Options) Options {
	if opt.Timeout <= 0 {
		opt.Timeout = 30 * time.Second
	}
	if opt.UserAgent == "" {
		opt.UserAgent = "autodocker/0.1"
	}
	return opt
}

// SetOptions 热更新运行时配置（加速主机、凭据、超时）。
//
// 只有当「主机集合」或「凭据」真的变了才丢弃缓存的 token ——
// token 是绑定到「某个主机 + 某个仓库」的，沿用旧 token 打新主机只会拿到 401。
// 但保存设置时往往只改了扫描间隔之类的无关项，那种情况下没必要让
// 已经换到的 token 白白作废。
func (c *Client) SetOptions(opt Options) {
	c.mu.Lock()
	defer c.mu.Unlock()

	next := withDefaults(opt)
	if !slices.Equal(c.opt.Mirrors, next.Mirrors) || !maps.Equal(c.opt.Auth, next.Auth) {
		c.tokens = map[string]tokenEntry{}
	}
	c.opt = next
}

// Mirrors 返回当前生效的加速主机列表（副本）。
func (c *Client) Mirrors() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.opt.Mirrors...)
}

// ManifestDigest 查询指定 tag 当前指向的 manifest digest。
// 返回的 digest 与 docker inspect 中 RepoDigests 的形态一致（多架构镜像为 index 摘要）。
func (c *Client) ManifestDigest(ctx context.Context, ref Reference) (string, error) {
	path := fmt.Sprintf("/v2/%s/manifests/%s", ref.Repository, ref.Tag)
	resp, err := c.do(ctx, http.MethodGet, ref, path, acceptManifest)
	if err != nil {
		return "", err
	}
	defer drainClose(resp)

	if d := resp.Header.Get("Docker-Content-Digest"); d != "" {
		return d, nil
	}
	// 兜底：少数注册表不返回该响应头，用响应体自算
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", fmt.Errorf("读取 manifest 响应失败: %w", err)
	}
	if len(body) == 0 {
		return "", errors.New("注册表返回空 manifest")
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Tags 拉取仓库的完整 tag 列表（自动翻页，最多 10 页）。
func (c *Client) Tags(ctx context.Context, ref Reference) ([]string, error) {
	path := fmt.Sprintf("/v2/%s/tags/list?n=1000", ref.Repository)
	var all []string
	seen := map[string]struct{}{}

	for page := 0; page < 10; page++ {
		resp, err := c.do(ctx, http.MethodGet, ref, path, "application/json")
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		next := nextPagePath(resp.Header.Get("Link"))
		resp.Body.Close()

		if readErr != nil {
			return nil, fmt.Errorf("读取 tag 列表失败: %w", readErr)
		}

		var payload struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("解析 tag 列表失败: %w", err)
		}
		for _, t := range payload.Tags {
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			all = append(all, t)
		}
		if next == "" {
			break
		}
		path = next
	}
	return all, nil
}

// Ping 探活：确认注册表可达且凭据有效。
func (c *Client) Ping(ctx context.Context, ref Reference) error {
	_, err := c.ManifestDigest(ctx, ref)
	return err
}

// ProbeResult 单个镜像源的探测结果，供设置页的「一键诊断」展示。
type ProbeResult struct {
	Host       string `json:"host"`
	Label      string `json:"label"`
	Mirror     bool   `json:"mirror"` // 是加速源还是直连
	OK         bool   `json:"ok"`
	ElapsedMS  int64  `json:"elapsed_ms"`
	Digest     string `json:"digest,omitempty"`
	Error      string `json:"error,omitempty"`
	ErrorBrief string `json:"error_brief,omitempty"`
}

// Probe 逐个探测该引用可用的镜像源，不做回退，如实报告每一条的连通情况。
//
// 设置页靠它做诊断：列表里全是「未知」时，一眼就能看出卡在哪一环
// —— 是加速站挂了、还是直连被墙、还是 tag 压根不存在，
// 不必让用户去翻容器日志。
func (c *Client) Probe(ctx context.Context, ref Reference) []ProbeResult {
	routes := c.routesFor(ref)
	path := fmt.Sprintf("/v2/%s/manifests/%s", ref.Repository, ref.Tag)
	out := make([]ProbeResult, 0, len(routes))

	for _, rt := range routes {
		start := time.Now()
		pr := ProbeResult{Host: rt.host, Label: rt.label, Mirror: rt.mirror}

		resp, err := c.doOne(ctx, http.MethodGet, rt, ref, path, acceptManifest)
		if err != nil {
			pr.Error = err.Error()
			pr.ErrorBrief = friendlyErr(err)
		} else {
			pr.Digest = resp.Header.Get("Docker-Content-Digest")
			drainClose(resp)
			pr.OK = true
		}
		pr.ElapsedMS = time.Since(start).Milliseconds()
		out = append(out, pr)
	}
	return out
}

// ---- 内部实现 ----

// route 一次请求的候选目标。
type route struct {
	host   string
	label  string
	mirror bool
}

// routesFor 给出某个引用可用的请求目标，按优先级排列。
//
// Docker Hub 先走配置的加速主机，直连排在最后：既是加速站全挂时的兜底，
// 也保证「没配加速」的场景与以前的行为一字不差。
// 非 Docker Hub 的上游一律直连 —— 公共加速站只缓存 Docker Hub。
func (c *Client) routesFor(ref Reference) []route {
	if !IsDockerHub(ref.Registry) {
		return []route{{host: ref.Registry, label: ref.Registry}}
	}

	c.mu.Lock()
	mirrors := append([]string(nil), c.opt.Mirrors...)
	c.mu.Unlock()

	out := make([]route, 0, len(mirrors)+1)
	for _, m := range mirrors {
		out = append(out, route{host: m, label: m, mirror: true})
	}
	out = append(out, route{host: DefaultRegistryAPI, label: DefaultRegistryAPI})
	return out
}

// do 请求注册表，在多个镜像源之间按序回退，返回第一个成功的响应。
//
// 失败时把**所有**源的失败原因聚合成一条错误，而不是只留下最后一个。
// 只报最后一个源很容易把人带偏：加速站超时、直连被拒，
// 真正有用的那条信息（比如「域名解析失败」）可能就藏在中间。
func (c *Client) do(ctx context.Context, method string, ref Reference, path, accept string) (*http.Response, error) {
	routes := c.routesFor(ref)
	attempts := make([]string, 0, len(routes))
	notFound, unauthorized := 0, 0

	for _, rt := range routes {
		if ctx.Err() != nil {
			break
		}
		resp, err := c.doOne(ctx, method, rt, ref, path, accept)
		if err == nil {
			return resp, nil
		}
		var de *deniedError
		switch {
		case errors.Is(err, ErrNotFound):
			notFound++
			attempts = append(attempts, rt.label+": 镜像或 tag 不存在")
		case errors.As(err, &de):
			attempts = append(attempts, rt.label+": "+de.Error())
		case errors.Is(err, ErrUnauthorized):
			unauthorized++
			attempts = append(attempts, rt.label+": 要求登录认证（HTTP 401），这个源不提供匿名读取")
		default:
			attempts = append(attempts, rt.label+": "+friendlyErr(err))
		}
	}

	switch {
	case len(attempts) == 0:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("没有可用的注册表地址")
	}
	return nil, summarizeFailures(attempts, notFound, unauthorized)
}

// summarizeFailures 把各镜像源的失败原因汇总成一条错误。
//
// 单独抽成纯函数是为了能被测到：do 本身会真的发网络请求，
// 而开发机的网络观测并不可靠（经常挂着代理），不适合写进断言。
//
// 只有「每个源给出的结论完全一致」时才收成哨兵值：全 404 才算不存在，
// 全 401 才算要登录。403 一律保留原文 —— 各家的理由不一样
// （限流、要付费通道、仓库没授权），压成一句话就没人知道该去干什么。
func summarizeFailures(attempts []string, notFound, unauthorized int) error {
	switch {
	case len(attempts) == 0:
		return errors.New("没有可用的注册表地址")
	case notFound == len(attempts):
		// 每个源都说不存在，那就是真不存在，没必要惊动用户去查网络
		return ErrNotFound
	case unauthorized == len(attempts):
		return ErrUnauthorized
	}
	return fmt.Errorf("所有镜像源均失败（%s）", strings.Join(attempts, "；"))
}

// doOne 向单个源发起请求，自动处理 Bearer 质询，401 时取 token 重试一次。
//
// 返回的 error 非 nil 时响应体已经关闭并抽干，调用方不必再管 resp。
func (c *Client) doOne(ctx context.Context, method string, rt route, ref Reference, path, accept string) (*http.Response, error) {
	target := c.endpoint(rt.host, path)

	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, target, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", c.userAgent())
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if tok := c.cachedToken(rt.host, ref.Repository); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}

		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			ch, ok := parseChallenge(resp.Header.Get("WWW-Authenticate"))
			drainClose(resp)
			if !ok {
				// 没有可用的质询信息，说明不是标准的 Bearer 流程
				return nil, ErrUnauthorized
			}
			token, terr := c.fetchToken(ctx, rt.host, ref.Repository, ch)
			if terr != nil {
				return nil, terr
			}
			c.storeToken(rt.host, ref.Repository, token)
			continue
		}

		switch resp.StatusCode {
		case http.StatusOK:
			return resp, nil
		case http.StatusUnauthorized:
			drainClose(resp)
			return nil, ErrUnauthorized
		case http.StatusForbidden:
			// 403 的响应体通常就是这家源自己写的理由，留着它比任何翻译都有用
			reason := bodySnippet(resp)
			return nil, &deniedError{reason: reason}
		case http.StatusNotFound:
			drainClose(resp)
			return nil, ErrNotFound
		case http.StatusTooManyRequests:
			drainClose(resp)
			return nil, errors.New("被限流（HTTP 429），稍后再试或换成别的加速源")
		default:
			code := resp.StatusCode
			drainClose(resp)
			return nil, fmt.Errorf("HTTP %d", code)
		}
	}
	return nil, ErrUnauthorized
}

// endpoint 拼接请求地址。host 已经是路由选定的实际目标主机。
func (c *Client) endpoint(host, path string) string {
	c.mu.Lock()
	insecure := c.opt.Insecure
	c.mu.Unlock()

	// 回环地址自动走 http，与 Docker 守护进程自身的默认规则对齐
	//（它把 127.0.0.0/8 与 localhost 当作安全源，无需改 daemon.json）。
	//
	// 这里必须独立兜一层：本地与私有注册表大多是 http 明文，若只认全局开关，
	// 用户为了测一个 127.0.0.1:5000 就得打开「允许 http 明文注册表」，
	// 而那会让 Docker Hub 与公共加速站也改发 http —— 它们都不支持，
	// 结果是整列状态变成「未知」，为测一个仓库赔上全机检测。
	scheme := "https"
	if insecure || isLoopbackHost(host) {
		scheme = "http"
	}
	return scheme + "://" + host + path
}

// isLoopbackHost 判断注册表主机是否为本机回环地址（允许带端口）。
//
// 只认回环，不认私有网段（192.168.x / 10.x）：Docker 守护进程的默认
// insecure 名单也只有回环，口径保持一致，用户不用猜两套规则。
func isLoopbackHost(host string) bool {
	h := hostWithoutPort(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// hostWithoutPort 去掉 "host:port" 里的端口。
//
// 多冒号且没有方括号的形式是 IPv6 本身（URL 规范要求带端口的 IPv6 必须写成
// [::1]:5000），不能拿最后一个冒号去切，否则 "::1" 会被切成一个冒号。
func hostWithoutPort(host string) string {
	if strings.HasPrefix(host, "[") {
		if end := strings.Index(host, "]"); end > 0 {
			return host[1:end]
		}
		return host
	}
	if strings.Count(host, ":") > 1 {
		return host
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		return host[:i]
	}
	return host
}

func (c *Client) userAgent() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opt.UserAgent
}

func (c *Client) credFor(host string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.opt.Auth) == 0 {
		return "", false
	}
	if v, ok := c.opt.Auth[host]; ok && v != "" {
		return v, true
	}
	// 用户凭据一般按 docker.io 书写，加速主机上再试一次这个别名
	if IsDockerHub(host) {
		for _, alias := range []string{DefaultRegistry, DefaultRegistryAPI} {
			if v, ok := c.opt.Auth[alias]; ok && v != "" {
				return v, true
			}
		}
	}
	return "", false
}

func (c *Client) cachedToken(host, repo string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.tokens[host+"|"+repo]
	if !ok || time.Now().After(e.expires) {
		return ""
	}
	return e.token
}

func (c *Client) storeToken(host, repo, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 匿名 token 一般 300 秒，留出安全边际
	c.tokens[host+"|"+repo] = tokenEntry{token: token, expires: time.Now().Add(4 * time.Minute)}
}

// challenge 注册表返回的认证质询。
type challenge struct {
	Scheme string
	Params map[string]string
}

// parseChallenge 解析形如：
//
//	Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"
func parseChallenge(header string) (*challenge, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil, false
	}
	scheme, rest, _ := strings.Cut(header, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return nil, false
	}
	params := map[string]string{}
	for _, part := range splitParams(rest) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		params[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	if params["realm"] == "" {
		return nil, false
	}
	return &challenge{Scheme: scheme, Params: params}, true
}

// splitParams 在尊重引号的前提下按逗号切分参数。
func splitParams(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// fetchToken 向认证服务换取 pull token。
//
// realm 一律取自质询本身：加速站会把自己配套的认证地址写在这里，
// 硬编码 Docker Hub 的 auth.docker.io 反而会在加速场景下走不通。
func (c *Client) fetchToken(ctx context.Context, host, repo string, ch *challenge) (string, error) {
	realm := ch.Params["realm"]
	q := url.Values{}
	if svc := ch.Params["service"]; svc != "" {
		q.Set("service", svc)
	}
	scope := ch.Params["scope"]
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	q.Set("scope", scope)

	target := realm
	if strings.Contains(target, "?") {
		target += "&" + q.Encode()
	} else {
		target += "?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", c.userAgent())
	if cred, ok := c.credFor(host); ok {
		if user, pass, found := strings.Cut(cred, ":"); found {
			req.SetBasicAuth(user, pass)
		} else {
			// 只配了 token 的私有仓库，按密码位递交
			req.SetBasicAuth("token", cred)
		}
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("换取注册表 token 失败: %w", err)
	}
	defer drainClose(resp)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("换取注册表 token 失败: HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("解析 token 响应失败: %w", err)
	}
	if payload.Token != "" {
		return payload.Token, nil
	}
	if payload.AccessToken != "" {
		return payload.AccessToken, nil
	}
	return "", errors.New("注册表返回的 token 为空")
}

// nextPagePath 从 Link 响应头中提取下一页路径。
//
// 形如：</v2/library/nginx/tags/list?last=1.25&n=1000>; rel="next"
func nextPagePath(link string) string {
	if link == "" {
		return ""
	}
	for _, part := range strings.Split(link, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start < 0 || end <= start {
			continue
		}
		return part[start+1 : end]
	}
	return ""
}

// ---- 辅助 ----

// drainClose 抽干并关闭响应体，保证连接能被复用。
func drainClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}

// bodySnippet 从错误响应里摘一句人话。
//
// 各家用得最多的是两种格式：registry 规范的 {"errors":[{"message":"…"}]}，
// 和反代直接甩一段 HTML（限流页、WAF 页）。前者取 message，后者剥标签，
// 剩下的原文截一段即可 —— 里面往往已经写清「免费节点当前繁忙」或「需要付费」，
// 这是用户唯一能照着做的线索。
func bodySnippet(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()

	var payload struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	text := ""
	if json.Unmarshal(raw, &payload) == nil {
		if payload.Message != "" {
			text = payload.Message
		} else if len(payload.Errors) > 0 {
			text = payload.Errors[0].Message
		}
	}
	if text == "" {
		text = stripTags(string(raw))
	}
	// 换行、连续空格压成单个空格：这句要嵌在错误汇总里，换行会把版面撑开。
	text = strings.Join(strings.Fields(text), " ")
	return truncateRunes(text, 120)
}

// stripTags 去掉 HTML 标签，只留文字。
func stripTags(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
			b.WriteByte(' ')
		case !inTag:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// truncateRunes 按字符截断。按字节切会把中文截成半个字，页面上显示成乱码。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// friendlyErr 把底层网络错误翻译成一句人话。
//
// 页面上的「未知」背后几乎都是这几种原因，直接铺原始英文错误
// 只会让人以为程序坏了，而不是网络不通。
func friendlyErr(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrNotFound) {
		return "镜像或 tag 不存在"
	}
	if errors.Is(err, ErrDenied) {
		var de *deniedError
		if errors.As(err, &de) {
			return de.Error()
		}
		return "镜像源拒绝访问（HTTP 403）"
	}
	if errors.Is(err, ErrUnauthorized) {
		return "要求登录认证（HTTP 401），这个源不提供匿名读取"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "请求超时"
	}
	if errors.Is(err, context.Canceled) {
		return "请求已取消"
	}

	s := err.Error()
	brief := ""
	switch {
	case strings.Contains(s, "no such host"):
		brief = "域名解析失败"
	case strings.Contains(s, "i/o timeout"):
		brief = "连接超时"
	case strings.Contains(s, "connection refused"):
		brief = "连接被拒绝"
	case strings.Contains(s, "connection reset"):
		brief = "连接被重置"
	case strings.Contains(s, "certificate"):
		brief = "TLS 证书校验失败"
	case strings.Contains(s, "EOF"):
		brief = "连接被中断"
	}
	if brief == "" {
		return truncate(s, 160)
	}
	return brief + "（" + truncate(s, 120) + "）"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Package dockerx 直接封装 Docker Engine REST API。
//
// 这里刻意不使用官方的 github.com/docker/docker/client：
//
//  1. 该 SDK 的 +incompatible 版本依赖 github.com/pkg/errors，而 pkg/errors
//     v0.9.1 的 go113.go 只写了旧式 `// +build go1.13` 约束。Go 1.26 起
//     这种约束不再被识别，该文件不参与编译，于是 errors.Is / errors.As
//     全部未定义，整个 client 包无法构建；
//  2. SDK 还会带进 opentelemetry、grpc、protobuf、go-connections 等一大批
//     间接依赖，对交叉编译与镜像体积都不友好。
//
// 本项目用到的端点很少，而且**容器配置可以按原始 JSON 原样搬运**——
// 这比经过一层类型定义更安全：不会因为 SDK 的类型过时而悄悄丢字段。
package dockerx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Client Docker Engine 客户端。
type Client struct {
	http *http.Client
	base string

	mu         sync.Mutex
	apiVersion string
	negotiated bool
}

// New 按 DOCKER_HOST 建立客户端。
//
// 支持 unix:///var/run/docker.sock、tcp://host:2375、http(s)://host:port。
func New(host string) (*Client, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}

	transport := &http.Transport{
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	var base string

	switch {
	case strings.HasPrefix(host, "unix://"):
		sock := strings.TrimPrefix(host, "unix://")
		if sock == "" {
			return nil, errors.New("DOCKER_HOST 的 unix 路径为空")
		}
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}
		base = "http://docker"
	case strings.HasPrefix(host, "tcp://"):
		base = "http://" + strings.TrimPrefix(host, "tcp://")
	case strings.HasPrefix(host, "http://"), strings.HasPrefix(host, "https://"):
		base = host
	case strings.HasPrefix(host, "npipe://"):
		// Windows 命名管道需要 go-winio，本项目只面向 Linux 部署，不引入这个依赖。
		return nil, errors.New(
			"DOCKER_HOST=npipe:// 暂不支持：本服务按 Linux 部署设计（挂载 docker.sock）。" +
				"若在 Windows 上调试，请改用 Docker Desktop 暴露的 tcp 端口（DOCKER_HOST=tcp://127.0.0.1:2375），" +
				"或在 WSL2 内运行本服务并使用 unix:///var/run/docker.sock")
	default:
		return nil, fmt.Errorf("不支持的 DOCKER_HOST: %s（支持 unix://、tcp://、http(s)://）", host)
	}

	return &Client{
		http: &http.Client{Transport: transport},
		base: strings.TrimRight(base, "/"),
	}, nil
}

// Close 释放空闲连接。
func (c *Client) Close() error {
	c.http.CloseIdleConnections()
	return nil
}

// Ping 探测守护进程，并在首次成功时完成 API 版本协商。
func (c *Client) Ping(ctx context.Context) error {
	var out struct {
		APIVersion string `json:"ApiVersion"`
		Version    string `json:"Version"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/version", nil, &out); err != nil {
		return err
	}
	c.mu.Lock()
	if out.APIVersion != "" {
		c.apiVersion = out.APIVersion
	}
	c.negotiated = true
	c.mu.Unlock()
	return nil
}

// RegistryMirrors 读取守护进程自己配置的镜像加速器。
//
// 对应 docker info 里的 Registry Mirrors，也就是 daemon.json 的
// registry-mirrors。本服务查询 digest 时是自己发起的 HTTP 请求，
// 并不会继承守护进程的代理与加速配置，于是很容易出现
// 「镜像拉得动、但检测全是未知」的怪象；把这份值读出来，
// 既能在诊断页里显示，也能让两者自动对齐。
func (c *Client) RegistryMirrors(ctx context.Context) ([]string, error) {
	var out struct {
		RegistryConfig struct {
			Mirrors      []string `json:"Mirrors"`
			IndexConfigs map[string]struct {
				Mirrors []string `json:"Mirrors"`
			} `json:"IndexConfigs"`
		} `json:"RegistryConfig"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/info", nil, &out); err != nil {
		return nil, fmt.Errorf("读取守护进程配置失败: %w", err)
	}

	set := map[string]struct{}{}
	var all []string
	add := func(list []string) {
		for _, m := range list {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			if _, dup := set[m]; dup {
				continue
			}
			set[m] = struct{}{}
			all = append(all, m)
		}
	}
	add(out.RegistryConfig.Mirrors)
	// 部分版本的守护进程不填顶层 Mirrors，只挂到 docker.io 的索引配置里
	if idx, ok := out.RegistryConfig.IndexConfigs["docker.io"]; ok {
		add(idx.Mirrors)
	}
	return all, nil
}

// ---- 对外数据结构 ----

// Container 本机容器的一个精简视图。
type Container struct {
	ID      string
	Name    string // 不含前导斜杠
	Image   string // 原始镜像引用，如 nginx:latest
	ImageID string
	State   string
	Labels  map[string]string
	Running bool
}

// Inspect 容器完整详情。Config / HostConfig 以原始 JSON 保存。
type Inspect struct {
	ID              string
	Name            string
	Image           string
	State           *State
	Config          map[string]any
	HostConfig      map[string]any
	NetworkSettings *NetworkSettings
	Mounts          []MountPoint
}

// State 容器运行状态。
type State struct {
	Running bool    `json:"Running"`
	Status  string  `json:"Status"`
	Health  *Health `json:"Health"`
}

// Health 健康检查状态。
type Health struct {
	Status string `json:"Status"`
}

// NetworkSettings 网络配置。
type NetworkSettings struct {
	Networks map[string]*EndpointSettings `json:"Networks"`
}

// EndpointSettings 单张网络上的接入设置。
type EndpointSettings struct {
	IPAMConfig *EndpointIPAMConfig `json:"IPAMConfig"`
	Links      []string            `json:"Links"`
	Aliases    []string            `json:"Aliases"`
	MacAddress string              `json:"MacAddress"`
	DriverOpts map[string]string   `json:"DriverOpts"`

	// 以下均为运行时字段，重建时必须剔除
	NetworkID  string `json:"NetworkID"`
	EndpointID string `json:"EndpointID"`
	Gateway    string `json:"Gateway"`
	IPAddress  string `json:"IPAddress"`
}

// EndpointIPAMConfig 静态地址配置。
type EndpointIPAMConfig struct {
	IPv4Address  string   `json:"IPv4Address,omitempty"`
	IPv6Address  string   `json:"IPv6Address,omitempty"`
	LinkLocalIPs []string `json:"LinkLocalIPs,omitempty"`
}

// MountPoint 一个实际挂载点（含由 VOLUME 隐式创建的匿名卷）。
type MountPoint struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Driver      string `json:"Driver"`
	Mode        string `json:"Mode"`
	RW          bool   `json:"RW"`
	Propagation string `json:"Propagation"`
}

// ---- 容器操作 ----

// ListContainers 列出全部容器（含已停止）。
func (c *Client) ListContainers(ctx context.Context) ([]Container, error) {
	var raw []struct {
		ID      string            `json:"Id"`
		Names   []string          `json:"Names"`
		Image   string            `json:"Image"`
		ImageID string            `json:"ImageID"`
		State   string            `json:"State"`
		Labels  map[string]string `json:"Labels"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/containers/json?all=1", nil, &raw); err != nil {
		return nil, fmt.Errorf("列举容器失败: %w", err)
	}

	out := make([]Container, 0, len(raw))
	for _, item := range raw {
		name := ""
		if len(item.Names) > 0 {
			name = strings.TrimPrefix(item.Names[0], "/")
		}
		out = append(out, Container{
			ID:      item.ID,
			Name:    name,
			Image:   item.Image,
			ImageID: item.ImageID,
			State:   item.State,
			Labels:  item.Labels,
			Running: item.State == "running",
		})
	}
	return out, nil
}

// InspectContainer 读取容器完整配置。
func (c *Client) InspectContainer(ctx context.Context, id string) (*Inspect, error) {
	var payload struct {
		ID              string           `json:"Id"`
		Name            string           `json:"Name"`
		Image           string           `json:"Image"`
		State           *State           `json:"State"`
		Config          map[string]any   `json:"Config"`
		HostConfig      map[string]any   `json:"HostConfig"`
		NetworkSettings *NetworkSettings `json:"NetworkSettings"`
		Mounts          []MountPoint     `json:"Mounts"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, &payload); err != nil {
		return nil, fmt.Errorf("读取容器详情失败: %w", err)
	}
	if payload.Config == nil {
		payload.Config = map[string]any{}
	}
	if payload.HostConfig == nil {
		payload.HostConfig = map[string]any{}
	}
	return &Inspect{
		ID:              payload.ID,
		Name:            strings.TrimPrefix(payload.Name, "/"),
		Image:           payload.Image,
		State:           payload.State,
		Config:          payload.Config,
		HostConfig:      payload.HostConfig,
		NetworkSettings: payload.NetworkSettings,
		Mounts:          payload.Mounts,
	}, nil
}

// CreateContainer 按给定配置创建容器，返回新容器 ID。
//
// cfg 为容器 Config 的原始键值（会被平铺到请求体顶层），
// hostCfg / netCfg 分别对应 HostConfig 与 NetworkingConfig。
func (c *Client) CreateContainer(ctx context.Context, cfg, hostCfg, netCfg map[string]any, name string) (string, error) {
	body := make(map[string]any, len(cfg)+2)
	for k, v := range cfg {
		body[k] = v
	}
	if hostCfg != nil {
		body["HostConfig"] = hostCfg
	}
	if len(netCfg) > 0 {
		body["NetworkingConfig"] = netCfg
	}

	path := "/containers/create"
	if name != "" {
		path += "?name=" + url.QueryEscape(name)
	}

	var out struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if err := c.doJSON(ctx, http.MethodPost, path, body, &out); err != nil {
		return "", fmt.Errorf("创建容器失败: %w", err)
	}
	if out.ID == "" {
		return "", errors.New("创建容器失败: 守护进程未返回容器 ID")
	}
	return out.ID, nil
}

// StartContainer 启动容器。
func (c *Client) StartContainer(ctx context.Context, id string) error {
	if err := c.doJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil); err != nil {
		return fmt.Errorf("启动容器失败: %w", err)
	}
	return nil
}

// StopContainer 停止容器。
func (c *Client) StopContainer(ctx context.Context, id string, timeoutSec int) error {
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	path := fmt.Sprintf("/containers/%s/stop?t=%d", url.PathEscape(id), timeoutSec)
	if err := c.doJSON(ctx, http.MethodPost, path, nil, nil); err != nil {
		return fmt.Errorf("停止容器失败: %w", err)
	}
	return nil
}

// RenameContainer 重命名容器。
func (c *Client) RenameContainer(ctx context.Context, id, newName string) error {
	path := "/containers/" + url.PathEscape(id) + "/rename?name=" + url.QueryEscape(newName)
	if err := c.doJSON(ctx, http.MethodPost, path, nil, nil); err != nil {
		return fmt.Errorf("重命名容器失败: %w", err)
	}
	return nil
}

// RemoveContainer 删除容器。
func (c *Client) RemoveContainer(ctx context.Context, id string, force, removeVolumes bool) error {
	path := fmt.Sprintf("/containers/%s?force=%d&v=%d", url.PathEscape(id), b2i(force), b2i(removeVolumes))
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, nil); err != nil {
		return fmt.Errorf("删除容器失败: %w", err)
	}
	return nil
}

// State 返回容器当前运行状态。
func (c *Client) State(ctx context.Context, id string) (running bool, health, status string, err error) {
	insp, err := c.InspectContainer(ctx, id)
	if err != nil {
		return false, "", "", err
	}
	if insp.State == nil {
		return false, "", "", nil
	}
	if insp.State.Health != nil {
		health = insp.State.Health.Status
	}
	return insp.State.Running, health, insp.State.Status, nil
}

// ---- 镜像操作 ----

// ImageRepoDigests 返回某个镜像的仓库摘要列表（形如 nginx@sha256:...）。
func (c *Client) ImageRepoDigests(ctx context.Context, refOrID string) ([]string, error) {
	var payload struct {
		RepoDigests []string `json:"RepoDigests"`
		RepoTags    []string `json:"RepoTags"`
	}
	path := "/images/" + url.PathEscape(refOrID) + "/json"
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &payload); err != nil {
		return nil, fmt.Errorf("读取镜像详情失败: %w", err)
	}
	return payload.RepoDigests, nil
}

// DigestForImage 从 RepoDigests 中挑出与指定引用匹配的那一条摘要。
//
// Docker 会在 RepoDigests 里保留该镜像所有打过的仓库地址，
// 需要按「仓库名」而非完整引用匹配，否则 nginx:latest 会因为 tag 不同而匹配失败。
func (c *Client) DigestForImage(ctx context.Context, imageID, reference string) (repo, digest string, err error) {
	digests, err := c.ImageRepoDigests(ctx, imageID)
	if err != nil {
		return "", "", err
	}
	want := normalizeRepo(stripTag(reference))
	for _, d := range digests {
		repoPart, digestPart, ok := strings.Cut(d, "@")
		if !ok {
			continue
		}
		if normalizeRepo(repoPart) == want {
			return repoPart, digestPart, nil
		}
	}
	if len(digests) > 0 {
		repoPart, digestPart, ok := strings.Cut(digests[0], "@")
		if ok {
			return repoPart, digestPart, nil
		}
	}
	return "", "", nil
}

// Image 本机镜像概要。
type Image struct {
	ID          string            // 镜像 ID（sha256:…）
	Tags        []string          // 打在这个镜像上的仓库引用，悬空镜像为空
	RepoDigests []string          // 摘要引用（name@sha256:…），悬空镜像通常为空
	Labels      map[string]string // 构建时写进配置的标签，是悬空镜像找回名字的唯一线索
	Size        int64             // 占用字节
	Created     int64             // 构建时间（unix 秒）
}

// ListImages 列出本机镜像。
//
// 悬空镜像（没有 tag 的那一层）也照样返回：它们不占 tag 但实实在在吃着磁盘，
// 是「清理镜像」最该清掉的一批，藏起来只会让人无从下手。
func (c *Client) ListImages(ctx context.Context) ([]Image, error) {
	var raw []struct {
		ID          string            `json:"Id"`
		RepoTags    []string          `json:"RepoTags"`
		RepoDigests []string          `json:"RepoDigests"`
		Labels      map[string]string `json:"Labels"`
		Size        int64             `json:"Size"`
		Created     int64             `json:"Created"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/images/json", nil, &raw); err != nil {
		return nil, fmt.Errorf("列举镜像失败: %w", err)
	}

	out := make([]Image, 0, len(raw))
	for _, item := range raw {
		tags := make([]string, 0, len(item.RepoTags))
		for _, t := range item.RepoTags {
			if t == "" || t == "<none>:<none>" || strings.HasSuffix(t, ":<none>") {
				continue
			}
			tags = append(tags, t)
		}
		sort.Strings(tags)
		out = append(out, Image{
			ID:          item.ID,
			Tags:        tags,
			RepoDigests: item.RepoDigests,
			Labels:      item.Labels,
			Size:        item.Size,
			Created:     item.Created,
		})
	}
	return out, nil
}

// PullImage 拉取镜像，并把进度流式回报。
//
// 必须把响应体读完，拉取才算真正结束。
func (c *Client) PullImage(ctx context.Context, reference string, logf func(format string, args ...any)) error {
	repo, tag := splitRef(reference)
	q := url.Values{}
	q.Set("fromImage", repo)
	if tag != "" {
		q.Set("tag", tag)
	}

	resp, err := c.raw(ctx, http.MethodPost, "/images/create?"+q.Encode(), nil, "")
	if err != nil {
		return fmt.Errorf("发起拉取失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("拉取失败: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	dec := json.NewDecoder(resp.Body)
	lastStatus := ""
	for {
		var msg struct {
			Status   string `json:"status"`
			Progress string `json:"progress"`
			Error    string `json:"error"`
		}
		err := dec.Decode(&msg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("读取拉取进度失败: %w", err)
		}
		if msg.Error != "" {
			return fmt.Errorf("拉取失败: %s", msg.Error)
		}
		if logf != nil && msg.Status != "" && msg.Status != lastStatus {
			lastStatus = msg.Status
			logf("拉取 %s: %s", reference, msg.Status)
		}
	}
	return nil
}

// RemoveImage 删除镜像。
func (c *Client) RemoveImage(ctx context.Context, imageID string, force bool) error {
	path := fmt.Sprintf("/images/%s?force=%d&noprune=0", url.PathEscape(imageID), b2i(force))
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, nil); err != nil {
		return fmt.Errorf("删除镜像失败: %w", err)
	}
	return nil
}

// ---- 内部 HTTP ----

// doJSON 发起一次 JSON 请求并解析响应。
func (c *Client) doJSON(ctx context.Context, method, path string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}

	resp, err := c.raw(ctx, method, path, body, "application/json")
	if err != nil {
		return err
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAPIError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// raw 发起请求，自动补齐 API 版本前缀。
func (c *Client) raw(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+c.versioned(path), body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", "autodocker/0.1")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}

	// 守护进程要求带版本前缀时（老版本会返回 400 + 提示），协商一次后重试
	if resp.StatusCode == http.StatusBadRequest && !c.isNegotiated() && !strings.HasPrefix(path, "/version") {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if perr := c.Ping(ctx); perr == nil {
			return c.raw(ctx, method, path, body, contentType)
		}
	}
	return resp, nil
}

func (c *Client) versioned(path string) string {
	c.mu.Lock()
	ver := c.apiVersion
	c.mu.Unlock()
	if ver == "" {
		return path
	}
	return "/v" + ver + path
}

func (c *Client) isNegotiated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.negotiated
}

// decodeAPIError 把守护进程返回的 {"message": "..."} 转成可读错误。
// APIError 守护进程返回的非 2xx 响应。
//
// 单独成类型，是因为调用方要能分辨具体状态：「这个容器已经不存在了」（404）
// 和「守护进程连不上」完全是两回事 —— 前者说明记录过期，后者说明环境坏了。
// 只看错误文本做字符串匹配的话，换个 Docker 版本、换种措辞就失效。
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Docker API %d: %s", e.Status, e.Message)
}

// IsNotFound 判断错误是不是「找不到这个对象」（HTTP 404）。
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

func decodeAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Message != "" {
		return &APIError{Status: resp.StatusCode, Message: payload.Message}
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	return &APIError{Status: resp.StatusCode, Message: msg}
}

// ---- 引用解析辅助 ----

// splitRef 把镜像引用拆成「仓库」与「tag」两部分，供 /images/create 使用。
func splitRef(ref string) (repo, tag string) {
	ref = strings.TrimSpace(ref)
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i], ""
	}
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, ""
}

// stripTag 去掉引用末尾的 tag，只保留仓库部分。
func stripTag(ref string) string {
	ref = strings.TrimSpace(ref)
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		return ref[:colon]
	}
	return ref
}

// normalizeRepo 归一化仓库名，便于比较（docker.io/library/nginx 与 nginx 视为同一个）。
func normalizeRepo(repo string) string {
	repo = strings.ToLower(strings.TrimSpace(repo))
	repo = strings.TrimPrefix(repo, "https://")
	repo = strings.TrimPrefix(repo, "http://")
	for _, prefix := range []string{"registry-1.docker.io/", "index.docker.io/", "docker.io/"} {
		if strings.HasPrefix(repo, prefix) {
			repo = strings.TrimPrefix(repo, prefix)
		}
	}
	if !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	return repo
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

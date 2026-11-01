package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultAPIBase 钉钉新版服务端 API 的基址。
// 抽成字段是为了单测能指向 httptest，不必真连钉钉。
const DefaultAPIBase = "https://api.dingtalk.com"

// Config 一套钉钉配置。
//
// 只有四个值需要用户填：应用凭据两个、卡片模板 ID、接收群会话 ID。
// 没有单独的 robotCode —— 非场景群的企业内部机器人发卡片时，
// 官方要求的 robotCode 就是应用的 Client ID（AppKey），没必要让人再抄一遍。
type Config struct {
	ClientID       string
	ClientSecret   string
	CardTemplateID string
	ConversationID string
	APIBase        string
}

// Normalized 补齐缺省值，返回一份可直接用的配置。
func (c Config) Normalized() Config {
	if strings.TrimSpace(c.APIBase) == "" {
		c.APIBase = DefaultAPIBase
	}
	c.APIBase = strings.TrimRight(c.APIBase, "/")
	c.ClientID = strings.TrimSpace(c.ClientID)
	c.ClientSecret = strings.TrimSpace(c.ClientSecret)
	c.CardTemplateID = strings.TrimSpace(c.CardTemplateID)
	c.ConversationID = strings.TrimSpace(c.ConversationID)
	return c
}

// HasCredentials 只要应用凭据就算「配了凭据」——
// 模板与会话可以后补，凭据是启动 Stream 长连接的前提。
func (c Config) HasCredentials() bool {
	return c.ClientID != "" && c.ClientSecret != ""
}

// Client 钉钉服务端 API 客户端。
//
// accessToken 有效期 7200 秒，必须缓存。钉钉对「付费 API 调用量」按次计量，
// 每个请求都重新取 token 会平白多消耗一倍额度，也更容易撞上 QPS 限制。
type Client struct {
	mu  sync.RWMutex
	cfg Config

	http *http.Client

	tkMu    sync.Mutex
	token   string
	expires time.Time
}

// New 构造客户端。
func New(cfg Config) *Client {
	cfg = cfg.Normalized()
	c := &Client{cfg: cfg}
	// 所有请求共用一个带超时的客户端：钉钉偶发慢响应时不能让调用方无限等，
	// 卡片更新的调用点都在扫描/回调链路上，卡住会连带阻塞后面的推送。
	c.http = &http.Client{Timeout: 20 * time.Second}
	return c
}

// SetOptions 换一份配置。换凭据必须清掉缓存 token ——
// 旧 token 属于旧应用，拿去调新应用的接口只会得到 403。
func (c *Client) SetOptions(cfg Config) {
	cfg = cfg.Normalized()
	c.mu.Lock()
	changed := c.cfg.ClientID != cfg.ClientID || c.cfg.ClientSecret != cfg.ClientSecret
	c.cfg = cfg
	c.mu.Unlock()

	if changed {
		c.tkMu.Lock()
		c.token = ""
		c.expires = time.Time{}
		c.tkMu.Unlock()
	}
}

// Config 返回当前配置的副本。
func (c *Client) Config() Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg
}

// Configured 应用凭据是否齐备。
func (c *Client) Configured() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.HasCredentials()
}

// ConversationID 当前配置的接收群会话 ID。
func (c *Client) ConversationID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.ConversationID
}

// CardTemplateID 当前配置的卡片模板 ID。
func (c *Client) CardTemplateID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.CardTemplateID
}

// Ready 发消息所必需的东西是否齐备（凭据 + 模板 + 会话）。
//
// 三者缺一都发不出去，但缺的东西不一样、提示也不该一样，
// 所以另外提供 MissingForSend 让调用方把原因说清楚。
func (c *Client) Ready() bool {
	return len(c.MissingForSend()) == 0
}

// MissingForSend 列出还缺哪些项，按「用户该先补哪个」排序。
func (c *Client) MissingForSend() []string {
	cfg := c.Config()
	var out []string
	if !cfg.HasCredentials() {
		out = append(out, "应用 Client ID / Client Secret")
	}
	if cfg.CardTemplateID == "" {
		out = append(out, "卡片模板 ID")
	}
	if cfg.ConversationID == "" {
		out = append(out, "接收群会话 ID")
	}
	return out
}

// AccessToken 取企业内部应用 accessToken，命中缓存时不发请求。
//
// 提前 5 分钟换新：token 失效刚好卡在请求中间时，钉钉返回的是
// 「invalidAuthentication」，看起来像凭据填错了，排查成本很高。
func (c *Client) AccessToken(ctx context.Context) (string, error) {
	c.tkMu.Lock()
	defer c.tkMu.Unlock()

	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}

	cfg := c.Config()
	if !cfg.HasCredentials() {
		return "", fmt.Errorf("钉钉应用凭据未配置")
	}

	body := map[string]string{"appKey": cfg.ClientID, "appSecret": cfg.ClientSecret}
	var out struct {
		AccessToken string `json:"accessToken"`
		ExpireIn    int    `json:"expireIn"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1.0/oauth2/accessToken", "", body, &out); err != nil {
		return "", fmt.Errorf("获取 accessToken 失败: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("获取 accessToken 失败: 返回为空")
	}

	ttl := time.Duration(out.ExpireIn) * time.Second
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	// 提前 5 分钟，但有效期本身很短时（测试环境）不能把余量吃掉
	margin := 5 * time.Minute
	if ttl <= margin*2 {
		margin = ttl / 4
	}
	c.token = out.AccessToken
	c.expires = time.Now().Add(ttl - margin)
	return c.token, nil
}

// Check 验证凭据能否换到 accessToken。
//
// 设置页的「检测连通性」用它：能拿到 token 就说明 Client ID / Secret 正确、
// 这台机器也能连上 api.dingtalk.com —— 这两件事是后面一切的前提。
func (c *Client) Check(ctx context.Context) error {
	_, err := c.AccessToken(ctx)
	return err
}

// CreateAndDeliverCard 创建卡片实例并投放到群会话。
//
// outTrackID 是这张卡片的唯一标识，后续更新它、以及按钮回调定位它，
// 用的都是这个值。调用方必须自己保证唯一（同一 ID 不能换内容重建）。
func (c *Client) CreateAndDeliverCard(ctx context.Context, outTrackID string, params map[string]string) error {
	cfg := c.Config()
	if !cfg.HasCredentials() {
		return fmt.Errorf("钉钉应用凭据未配置")
	}
	if cfg.CardTemplateID == "" {
		return fmt.Errorf("尚未填写卡片模板 ID")
	}
	if cfg.ConversationID == "" {
		return fmt.Errorf("尚未配置接收群会话 ID")
	}

	body := map[string]any{
		"cardTemplateId": cfg.CardTemplateID,
		"outTrackId":     outTrackID,
		// STREAM：按钮点击通过 Stream 长连接回传给我们。
		// 写成 HTTP 就得有公网地址，整个「免公网」的前提就没了。
		"callbackType": "STREAM",
		"cardData":     map[string]any{"cardParamMap": params},
		// 群聊场域：dtv1.card//IM_GROUP.<openConversationId>
		"openSpaceId":           "dtv1.card//IM_GROUP." + cfg.ConversationID,
		"imGroupOpenSpaceModel": map[string]any{"supportForward": true},
		"imGroupOpenDeliverModel": map[string]any{
			// 非场景群的企业内部机器人，官方要求这里填 AppKey，也就是 Client ID
			"robotCode": cfg.ClientID,
		},
	}

	var out struct {
		Success bool `json:"success"`
		Result  struct {
			OutTrackID     string `json:"outTrackId"`
			DeliverResults []struct {
				SpaceType string `json:"spaceType"`
				SpaceID   string `json:"spaceId"`
				Success   bool   `json:"success"`
				ErrorMsg  string `json:"errorMsg"`
			} `json:"deliverResults"`
		} `json:"result"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1.0/card/instances/createAndDeliver", "", body, &out); err != nil {
		return fmt.Errorf("投放钉钉卡片失败: %w", err)
	}
	// 顶层 success 为 true 但某个场域投放失败的情况是存在的，
	// 只判顶层会把「卡片其实没进群」当成功，用户干等一张永远不来的卡片。
	for _, r := range out.Result.DeliverResults {
		if !r.Success {
			msg := r.ErrorMsg
			if msg == "" {
				msg = "未知原因"
			}
			return fmt.Errorf("卡片投放失败（%s）: %s", r.SpaceType, msg)
		}
	}
	return nil
}

// UpdateCard 更新已有卡片的数据。
//
// updateCardDataByKey=true 表示按 key 局部覆盖：只传要改的变量，
// 模板里其它变量保持原值。这是钉钉与飞书最不一样的一处 ——
// 飞书是整张卡片换掉，「已完成」之后按钮自然消失；
// 钉钉只改数据不改结构，按钮会留在卡片上（再点会提示已在进行中，不会重复执行）。
func (c *Client) UpdateCard(ctx context.Context, outTrackID string, params map[string]string) error {
	cfg := c.Config()
	if !cfg.HasCredentials() {
		return fmt.Errorf("钉钉应用凭据未配置")
	}
	if outTrackID == "" {
		return fmt.Errorf("卡片 ID 为空")
	}

	body := map[string]any{
		"outTrackId":     outTrackID,
		"cardTemplateId": cfg.CardTemplateID,
		"cardData":       map[string]any{"cardParamMap": params},
		"cardUpdateOptions": map[string]any{
			"updateCardDataByKey": true,
		},
	}
	if err := c.doJSON(ctx, http.MethodPut, "/v1.0/card/instances", "", body, nil); err != nil {
		return fmt.Errorf("更新钉钉卡片失败: %w", err)
	}
	return nil
}

// apiError 钉钉的错误响应：HTTP 状态码之外还带一个业务 code，
// 只报状态码说不出「模板没发布」「权限没开」这类具体原因。
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string {
	switch {
	case e.code != "" && e.message != "":
		return fmt.Sprintf("HTTP %d %s: %s", e.status, e.code, e.message)
	case e.message != "":
		return fmt.Sprintf("HTTP %d: %s", e.status, e.message)
	default:
		return fmt.Sprintf("HTTP %d", e.status)
	}
}

// doJSON 发一次请求并把响应解到 out（out 可为 nil）。
func (c *Client) doJSON(ctx context.Context, method, path, token string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}

	cfg := c.Config()
	req, err := http.NewRequestWithContext(ctx, method, cfg.APIBase+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// 只有换 accessToken 与 Stream 注册这两个接口不需要带 token：
	// 前者就是去拿 token，后者用 body 里的 clientSecret 认证。
	// 其余接口若不带 token，钉钉统一返回 401，报错信息还很容易被误解成凭据填错。
	needAuth := path != "/v1.0/oauth2/accessToken" && !strings.HasPrefix(path, "/v1.0/gateway/connections/open")
	if needAuth {
		if token == "" {
			token, err = c.AccessToken(ctx)
			if err != nil {
				return err
			}
		}
		req.Header.Set("x-acs-dingtalk-access-token", token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("请求钉钉失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("读取钉钉响应失败: %w", err)
	}

	if resp.StatusCode >= 400 {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(raw))
			if len(e.Message) > 300 {
				e.Message = e.Message[:300]
			}
		}
		return &apiError{status: resp.StatusCode, code: e.Code, message: e.Message}
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("解析钉钉响应失败: %w", err)
	}
	return nil
}

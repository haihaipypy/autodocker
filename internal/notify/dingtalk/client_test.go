package dingtalk

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// accessToken 必须缓存。
//
// 钉钉按「付费 API 调用量」计费，每次发卡片都重新换 token 会把这笔用量翻倍，
// 也更容易撞上 QPS 限制。这一条用请求计数钉死，改回「每次都取」会立刻红。
func TestAccessTokenCached(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "tk-1", "expireIn": 7200})
	}))
	defer srv.Close()

	c := New(Config{ClientID: "id", ClientSecret: "sec", APIBase: srv.URL})
	for i := 0; i < 3; i++ {
		tok, err := c.AccessToken(context.Background())
		if err != nil {
			t.Fatalf("第 %d 次取 token 失败: %v", i+1, err)
		}
		if tok != "tk-1" {
			t.Fatalf("token = %q，期望 tk-1", tok)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("accessToken 被请求了 %d 次，期望 1 次（必须缓存）", got)
	}
}

// 换凭据必须清缓存：旧 token 属于旧应用，拿去调新应用只会 403。
func TestSetOptionsClearsToken(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "tk-" + string(rune('0'+n)), "expireIn": 7200})
	}))
	defer srv.Close()

	c := New(Config{ClientID: "a", ClientSecret: "s1", APIBase: srv.URL})
	if _, err := c.AccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 只改会话不该清 token
	c.SetOptions(Config{ClientID: "a", ClientSecret: "s1", ConversationID: "cid", APIBase: srv.URL})
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("只改会话就重新取了 token（%d 次）", got)
	}
	// 换凭据必须清
	c.SetOptions(Config{ClientID: "b", ClientSecret: "s2", APIBase: srv.URL})
	if _, err := c.AccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("换凭据后没有重新取 token（共 %d 次），会拿着旧应用的 token 调新应用", got)
	}
}

// 投放卡片的请求体必须符合钉钉要求，尤其是三处最容易写错的：
// callbackType 为 STREAM、openSpaceId 的 dtv1.card//IM_GROUP. 前缀、robotCode 用 ClientID。
func TestCreateAndDeliverCardBody(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.0/oauth2/accessToken" {
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "tk", "expireIn": 7200})
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": map[string]any{
				"outTrackId":     "ad-1-2",
				"deliverResults": []any{map[string]any{"spaceType": "IM_GROUP", "success": true}},
			},
		})
	}))
	defer srv.Close()

	c := New(Config{
		ClientID: "ding_client", ClientSecret: "sec",
		CardTemplateID: "tpl.schema", ConversationID: "cid123", APIBase: srv.URL,
	})
	if err := c.CreateAndDeliverCard(context.Background(), "ad-1-2", CardParams{VarTitle: "t"}); err != nil {
		t.Fatalf("投放失败: %v", err)
	}

	if got["callbackType"] != "STREAM" {
		t.Fatalf("callbackType = %v，必须是 STREAM（写成 HTTP 就要公网地址）", got["callbackType"])
	}
	if got["cardTemplateId"] != "tpl.schema" || got["outTrackId"] != "ad-1-2" {
		t.Fatalf("模板或卡片 ID 不对: %v / %v", got["cardTemplateId"], got["outTrackId"])
	}
	if got["openSpaceId"] != "dtv1.card//IM_GROUP.cid123" {
		t.Fatalf("openSpaceId = %v，群聊必须是 dtv1.card//IM_GROUP.<会话ID>", got["openSpaceId"])
	}
	model, _ := got["imGroupOpenDeliverModel"].(map[string]any)
	if model["robotCode"] != "ding_client" {
		t.Fatalf("robotCode = %v，非场景群企业内部机器人要填 AppKey（Client ID）", model["robotCode"])
	}
}

// 某个场域投放失败但顶层 success=true 是真实存在的情况。
// 只判顶层会把「卡片其实没进群」当成功，用户干等一张永远不来的卡片。
func TestCreateAndDeliverPartialFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.0/oauth2/accessToken" {
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "tk", "expireIn": 7200})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": map[string]any{
				"deliverResults": []any{map[string]any{
					"spaceType": "IM_GROUP", "success": false, "errorMsg": "机器人不在该群",
				}},
			},
		})
	}))
	defer srv.Close()

	c := New(Config{ClientID: "id", ClientSecret: "s", CardTemplateID: "tpl", ConversationID: "cid", APIBase: srv.URL})
	err := c.CreateAndDeliverCard(context.Background(), "ad-1-2", CardParams{})
	if err == nil || !strings.Contains(err.Error(), "机器人不在该群") {
		t.Fatalf("场域投放失败必须报错并带上原因，得到 %v", err)
	}
}

func TestUpdateCardUsesPut(t *testing.T) {
	var method, path string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.0/oauth2/accessToken" {
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "tk", "expireIn": 7200})
			return
		}
		method, path = r.Method, r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(Config{ClientID: "id", ClientSecret: "s", CardTemplateID: "tpl", APIBase: srv.URL})
	if err := c.UpdateCard(context.Background(), "ad-9-1", CardParams{VarStatus: "更新完成"}); err != nil {
		t.Fatalf("更新卡片失败: %v", err)
	}
	if method != http.MethodPut || path != "/v1.0/card/instances" {
		t.Fatalf("更新卡片要用 PUT /v1.0/card/instances，实际 %s %s", method, path)
	}
	if got["outTrackId"] != "ad-9-1" {
		t.Fatalf("outTrackId = %v", got["outTrackId"])
	}
	opts, _ := got["cardUpdateOptions"].(map[string]any)
	if opts["updateCardDataByKey"] != true {
		t.Fatalf("必须按 key 局部覆盖，否则未传的变量会被清空: %v", opts)
	}
}

// 钉钉的错误细节藏在响应体的 code/message 里，只报 HTTP 状态码
// 会让「模板没发布」这种可自修的问题变成一头雾水。
func TestAPIErrorCarriesCodeAndMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.0/oauth2/accessToken" {
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "tk", "expireIn": 7200})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"param.templateUnpublished","message":"模板未发布"}`))
	}))
	defer srv.Close()

	c := New(Config{ClientID: "id", ClientSecret: "s", CardTemplateID: "tpl", ConversationID: "cid", APIBase: srv.URL})
	err := c.CreateAndDeliverCard(context.Background(), "ad-1", CardParams{})
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "param.templateUnpublished") || !strings.Contains(err.Error(), "模板未发布") {
		t.Fatalf("错误里必须同时带业务码与原文，得到: %v", err)
	}
}

// 缺什么要说清楚缺什么：三种缺法的补法完全不同，
// 一句笼统的「未配置」会让人从头翻一遍。
func TestMissingForSend(t *testing.T) {
	c := New(Config{})
	if got := c.MissingForSend(); len(got) != 3 {
		t.Fatalf("全空时应缺 3 项，得到 %v", got)
	}
	c = New(Config{ClientID: "id", ClientSecret: "s", CardTemplateID: "tpl"})
	got := c.MissingForSend()
	if len(got) != 1 || !strings.Contains(got[0], "会话") {
		t.Fatalf("只差会话时应只报会话，得到 %v", got)
	}
	c = New(Config{ClientID: "id", ClientSecret: "s", CardTemplateID: "tpl", ConversationID: "cid"})
	if !c.Ready() {
		t.Fatalf("四项齐全应就绪，仍缺 %v", c.MissingForSend())
	}
}

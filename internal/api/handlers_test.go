package api

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/haihaipypy/autodocker/internal/dockerx"
)

// 「未使用镜像」这格的口径：按镜像 ID 去重、只要还有容器引用就不算。
// 数错了用户会照着去清理，结果是删了在用的镜像 —— 所以钉死在这里。
func TestCountUnusedImages(t *testing.T) {
	images := []dockerx.Image{
		{ID: "sha256:aaa", Tags: []string{"nginx:latest"}},         // 容器在用
		{ID: "sha256:bbb", Tags: []string{"redis:7", "redis:7.2"}}, // 没人用，两个 tag 算一个
		{ID: "sha256:ccc"}, // 悬空层，没人用，要算
		{ID: ""},           // 拿不到 ID 的读不懂，跳过
	}
	used := map[string]bool{"sha256:aaa": true}

	if got := countUnusedImages(images, used); got != 2 {
		t.Errorf("未使用镜像应为 2（redis + 悬空层），实际 %d", got)
	}

	// 同一 ID 重复出现（响应里有重复项时）不能数两遍
	dup := []dockerx.Image{{ID: "sha256:ddd"}, {ID: "sha256:ddd"}}
	if got := countUnusedImages(dup, nil); got != 1 {
		t.Errorf("同一镜像出现两次应只算一个，实际 %d", got)
	}

	// 全部在用 → 0
	if got := countUnusedImages(images[:1], used); got != 0 {
		t.Errorf("容器在用的镜像不该计入，实际 %d", got)
	}

	// 没有镜像 → 0
	if got := countUnusedImages(nil, used); got != 0 {
		t.Errorf("空列表应为 0，实际 %d", got)
	}
}

// 推送接口的鉴定：令牌没生成时一律不放行。
//
// 这里最容易写错的一版是「两边都为空就算匹配」—— 那等于令牌没生成时
// 接口对所有人敞开，而用户以为自己什么都没开。
func TestMatchPushToken(t *testing.T) {
	const want = "0123456789abcdef0123456789abcdef"

	req := func(pairs ...string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/push", nil)
		for i := 0; i+1 < len(pairs); i += 2 {
			r.Header.Set(pairs[i], pairs[i+1])
		}
		return r
	}

	cases := []struct {
		name string
		req  *http.Request
		want string
		ok   bool
	}{
		{"正确令牌", req("X-Push-Token", want), want, true},
		{"令牌首尾空格", req("X-Push-Token", "  "+want+"  "), want, true},
		{"Bearer 写法", req("Authorization", "Bearer "+want), want, true},
		{"bearer 小写", req("Authorization", "bearer "+want), want, true},
		{"令牌错误", req("X-Push-Token", "ffffffffffffffffffffffffffffffff"), want, false},
		{"没带令牌", req(), want, false},
		{"只带 Basic", req("Authorization", "Basic YWRtaW46eA=="), want, false},
		{"接口未启用时带什么都拒绝", req("X-Push-Token", ""), "", false},
		{"接口未启用时带空串也拒绝", req(), "", false},
	}
	for _, c := range cases {
		if got := matchPushToken(c.req, c.want); got != c.ok {
			t.Errorf("%s: matchPushToken = %v，期望 %v", c.name, got, c.ok)
		}
	}
}

// 令牌必须够长、够随机、每次都不一样。
func TestNewPushToken(t *testing.T) {
	a, err := newPushToken()
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if len(a) != pushTokenBytes*2 {
		t.Fatalf("长度应为 %d 个 hex 字符，得到 %d", pushTokenBytes*2, len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("不是合法 hex: %v", err)
	}
	b, _ := newPushToken()
	if a == b {
		t.Fatal("两次生成不该相同")
	}
}

// 推送接口不走网页登录，但**只放行它自己**。
//
// 令牌的生成与撤销必须留在登录后面：放行范围一旦写成前缀匹配（/api/push），
// 任何人不登录就能把令牌换掉 —— 这比令牌泄露还糟，等于直接接管了推送口。
func TestPushPathSkipsBasicAuthOnlyItself(t *testing.T) {
	s := &Server{}
	s.setAuth("admin", "secret")

	reached := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // 走到这里说明被放行了
	})
	h := s.withAuth(reached)

	cases := []struct {
		path string
		auth bool
		want int
	}{
		{"/api/push", false, http.StatusTeapot},
		{"/api/push", true, http.StatusTeapot},
		{"/api/push/token", false, http.StatusUnauthorized},
		{"/api/push/token", true, http.StatusTeapot},
		{"/api/settings", false, http.StatusUnauthorized},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, c.path, nil)
		if c.auth {
			req.SetBasicAuth("admin", "secret")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s auth=%v: 状态码 %d，期望 %d", c.path, c.auth, rec.Code, c.want)
		}
	}
}

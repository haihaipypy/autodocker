package dockerx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 「找不到这个对象」必须能被程序分辨出来。
//
// 上层要据此决定兜底方式：容器编号失效（404）可以按容器名找回来，
// 守护进程连不上则只能报错。用字符串匹配 "No such container" 是行不通的 ——
// 措辞随 Docker 版本变，而且别的接口也会返回 404。
func TestDecodeAPIErrorMarksNotFound(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader(`{"message":"No such container: abc123"}`)),
	}
	err := decodeAPIError(resp)
	if !IsNotFound(err) {
		t.Fatalf("404 应判定为未找到，得到 %v", err)
	}
	// 原文案不能丢：日志里要能看到守护进程到底说了什么。
	if !strings.Contains(err.Error(), "No such container: abc123") {
		t.Fatalf("错误信息应保留服务端原文，得到 %q", err.Error())
	}

	server := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(`{"message":"driver failed"}`)),
	}
	if IsNotFound(decodeAPIError(server)) {
		t.Fatal("500 不该判定为未找到")
	}
}

func TestIsNotFoundIgnoresOtherErrors(t *testing.T) {
	if IsNotFound(fmt.Errorf("Docker API 404: No such container")) {
		t.Fatal("普通 error 不该被当成 404：只有 decodeAPIError 产出的才算")
	}
	if IsNotFound(nil) {
		t.Fatal("nil 不该判定为未找到")
	}
	var apiErr *APIError
	if !errors.As(&APIError{Status: http.StatusNotFound, Message: "x"}, &apiErr) {
		t.Fatal("APIError 应可被 errors.As 取到")
	}
}

// 拿镜像 ID 去查「它身上还有哪些仓库名」：这是把「用 ID 起的容器」救回可检测状态的
// 唯一线索，拉错字段（比如只取 RepoDigests）就白改一场。
func TestImageNamesReadsTagsAndDigests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/sha256:abc/json" {
			t.Errorf("请求路径不对: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"RepoTags":["chaitin/safeline-mgt:9.4.7"],`+
			`"RepoDigests":["chaitin/safeline-mgt@sha256:734f"]}`)
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	tags, digests, err := c.ImageNames(context.Background(), "sha256:abc")
	if err != nil {
		t.Fatalf("ImageNames: %v", err)
	}
	if len(tags) != 1 || tags[0] != "chaitin/safeline-mgt:9.4.7" {
		t.Fatalf("RepoTags 没读到: %v", tags)
	}
	if len(digests) != 1 {
		t.Fatalf("RepoDigests 没读到: %v", digests)
	}

	// 悬空镜像：两个列表都是 null，不能当成错误
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"RepoTags":null,"RepoDigests":null}`)
	}))
	defer srv2.Close()
	c2, _ := New(srv2.URL)
	defer c2.Close()
	tags, digests, err = c2.ImageNames(context.Background(), "deadbeef")
	if err != nil || len(tags) != 0 || len(digests) != 0 {
		t.Fatalf("悬空镜像应返回空列表且无错，得到 %v %v %v", tags, digests, err)
	}
}

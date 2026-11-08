package dockerx

import (
	"errors"
	"fmt"
	"io"
	"net/http"
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

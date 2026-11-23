package dockerx

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 读宿主机文件全靠这个接口：tar 流里第一个普通文件就是它。
func TestCopyFromContainerReadsFile(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/abc/archive" {
			t.Errorf("路径不对: %s", r.URL.Path)
		}
		gotPath = r.URL.Query().Get("path")
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{Name: "docker-compose.yaml", Mode: 0o640, Size: 5, Uid: 1000, Gid: 1000, Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte("hello"))
		_ = tw.Close()
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	body, meta, err := c.CopyFromContainer(context.Background(), "abc", "/data/safeline/docker-compose.yaml")
	if err != nil {
		t.Fatalf("CopyFromContainer: %v", err)
	}
	if gotPath != "/data/safeline/docker-compose.yaml" {
		t.Errorf("path 参数没传对: %s", gotPath)
	}
	if string(body) != "hello" {
		t.Errorf("内容不对: %q", body)
	}
	if meta.Mode != 0o640 {
		t.Errorf("权限位没带回来: %o", meta.Mode)
	}
	// 属主也要带回来：以 root 写回时 tar 头里的 uid/gid 就是新属主。
	if meta.UID != 1000 || meta.GID != 1000 {
		t.Errorf("属主没带回来: %d:%d", meta.UID, meta.GID)
	}
}

// 文件不存在时要说人话，而不是抛一个空内容出去 —— 空内容写回去会把配置清空。
func TestCopyFromContainerNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Could not find the file /nope"}`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	defer c.Close()

	if _, _, err := c.CopyFromContainer(context.Background(), "abc", "/nope"); err == nil {
		t.Fatal("应当报错")
	}
}

// 写回去的是「一个名为 .env 的 tar」，路径参数是目录。
func TestCopyToContainerWritesFile(t *testing.T) {
	var gotDir, gotName, gotBody string
	var gotUID, gotGID int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("方法不对: %s", r.Method)
		}
		gotDir = r.URL.Query().Get("path")
		tr := tar.NewReader(r.Body)
		hdr, err := tr.Next()
		if err != nil {
			t.Errorf("不是合法 tar: %v", err)
			return
		}
		gotName = hdr.Name
		gotUID, gotGID = hdr.Uid, hdr.Gid
		b, _ := io.ReadAll(tr)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	defer c.Close()

	if err := c.CopyToContainer(context.Background(), "abc", "/data/safeline", ".env", []byte("IMAGE_TAG=9.4.8\n"), FileMeta{Mode: 0o600, UID: 1000, GID: 1000}); err != nil {
		t.Fatalf("CopyToContainer: %v", err)
	}
	if gotDir != "/data/safeline" || gotName != ".env" {
		t.Errorf("落点不对: dir=%s name=%s", gotDir, gotName)
	}
	if gotBody != "IMAGE_TAG=9.4.8\n" {
		t.Errorf("内容不对: %q", gotBody)
	}
	if gotUID != 1000 || gotGID != 1000 {
		t.Errorf("属主没写回: %d:%d", gotUID, gotGID)
	}
}

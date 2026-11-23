package dockerx

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// maxConfigFile 单个配置文件的大小上限。compose / .env 都是几十 KB 的东西，
// 给到 4MB 只是防一手「传了个镜像进来」。
const maxConfigFile = 4 << 20

// FileMeta 文件的权限位与属主。
//
// 属主必须一起带回来：以 root 写文件时 tar 头里的 uid/gid 就是新的属主，
// 把原本属于 1000 的 .env 变成 root:root 0600，之后非 root 跑 compose 就读不到了。
type FileMeta struct {
	Mode int64
	UID  int
	GID  int
}

// CopyFromContainer 读出容器里的一个文件，同时带回权限位。
//
// 借的是 /containers/{id}/archive：路径在容器的挂载空间里解析，
// 所以「临时起一个挂了宿主机目录的容器」就能读到宿主机上的文件 ——
// 本程序自己跑在容器里，宿主机的 /data/xxx 在它这边根本不存在。
// 读出来的权限位要留着：写回去时按原样还，别把 0600 的 .env 变成 0644。
func (c *Client) CopyFromContainer(ctx context.Context, id, path string) ([]byte, FileMeta, error) {
	resp, err := c.raw(ctx, http.MethodGet,
		"/containers/"+url.PathEscape(id)+"/archive?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		return nil, FileMeta{}, fmt.Errorf("读取文件 %s 失败: %w", path, err)
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, FileMeta{}, decodeAPIError(resp)
	}

	tr := tar.NewReader(resp.Body)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, FileMeta{}, fmt.Errorf("解析文件 %s 失败: %w", path, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		buf, err := io.ReadAll(io.LimitReader(tr, maxConfigFile))
		if err != nil {
			return nil, FileMeta{}, fmt.Errorf("读取文件 %s 失败: %w", path, err)
		}
		return buf, FileMeta{Mode: hdr.Mode, UID: hdr.Uid, GID: hdr.Gid}, nil
	}
	return nil, FileMeta{}, fmt.Errorf("容器里没有 %s 这个文件", path)
}

// CopyToContainer 覆盖容器里的一个文件（dir + name 决定落点）。
func (c *Client) CopyToContainer(ctx context.Context, id, dir, name string, content []byte, meta FileMeta) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     meta.Mode,
		Uid:      meta.UID,
		Gid:      meta.GID,
		Size:     int64(len(content)),
		ModTime:  time.Now(),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return fmt.Errorf("打包文件 %s 失败: %w", name, err)
	}
	if _, err := tw.Write(content); err != nil {
		return fmt.Errorf("打包文件 %s 失败: %w", name, err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("打包文件 %s 失败: %w", name, err)
	}

	resp, err := c.raw(ctx, http.MethodPut,
		"/containers/"+url.PathEscape(id)+"/archive?path="+url.QueryEscape(dir), &buf, "application/x-tar")
	if err != nil {
		return fmt.Errorf("写入文件 %s 失败: %w", name, err)
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAPIError(resp)
	}
	return nil
}

package hub

import (
	"context"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// osHostname 单独包一层，便于测试时替换。
func osHostname() (string, error) { return os.Hostname() }

// shortID 截短容器编号用于展示。取 12 位是 Docker 自己的惯例。
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// osName 取目标机的系统名。
//
// 走的是守护进程的 /info，而不是容器里的 /etc/os-release —— 后者读到的是
// 基础镜像（alpine），对用户毫无意义；用户想知道的是「这台是群晖还是飞牛」。
// 读不到就退回 GOOS，至少不空着。
func (c *Client) osName() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	name, _, _, err := c.dk.HostInfo(ctx)
	if err != nil || strings.TrimSpace(name) == "" {
		return runtime.GOOS
	}
	return name
}

// Ring 是一个按行存放的日志环形缓冲。
//
// 挂在 slog 的输出上，每次上报顺带把最近若干行带过去，用户在 Hub 页面上
// 就能看出节点出了什么事，不用 SSH 登进去翻 docker logs。
type Ring struct {
	mu    sync.Mutex
	lines []string
	max   int
}

// NewRing 构造环形缓冲。
func NewRing(max int) *Ring {
	if max <= 0 {
		max = 200
	}
	return &Ring{max: max}
}

// Write 实现 io.Writer，直接接进 slog 的 MultiWriter。
func (r *Ring) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\r\n")
	if line != "" {
		r.mu.Lock()
		r.lines = append(r.lines, line)
		if len(r.lines) > r.max {
			r.lines = append([]string(nil), r.lines[len(r.lines)-r.max:]...)
		}
		r.mu.Unlock()
	}
	return len(p), nil
}

// Tail 取最近 n 行。
func (r *Ring) Tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.lines) {
		n = len(r.lines)
	}
	out := make([]string, n)
	copy(out, r.lines[len(r.lines)-n:])
	return out
}

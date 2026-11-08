// Package api 暴露 REST 接口并同源提供前端页面。
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/haihaipypy/autodocker/internal/config"
	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/notify"
	"github.com/haihaipypy/autodocker/internal/registry"
	"github.com/haihaipypy/autodocker/internal/scanner"
	"github.com/haihaipypy/autodocker/internal/selfcheck"
	"github.com/haihaipypy/autodocker/internal/store"
	"github.com/haihaipypy/autodocker/internal/updater"
	"github.com/haihaipypy/autodocker/web"
)

// Deps 服务的全部依赖。
type Deps struct {
	Cfg      *config.Config
	Store    *store.Store
	Scanner  *scanner.Scanner
	Updater  *updater.Updater
	Notify   *notify.Service
	Registry *registry.Client
	Docker   *dockerx.Client
	Self     *selfcheck.Checker
	Logger   *slog.Logger
}

// Server HTTP 服务。
type Server struct {
	cfg  *config.Config
	st   *store.Store
	sc   *scanner.Scanner
	up   *updater.Updater
	nt   *notify.Service
	reg  *registry.Client
	dk   *dockerx.Client
	self *selfcheck.Checker
	log  *slog.Logger

	// 访问密码可以在设置页随时改，所以不能直接读 cfg —— 那是启动时定死的。
	authMu   sync.RWMutex
	authUser string
	authPass string

	// 首页「未使用镜像」那格要问 Docker 才知道，而 stats 是 5 秒一轮的轮询，
	// 用一层短缓存兜住，别每轮都去翻一遍镜像与容器列表。
	// unusedOK = 上次是否真的读到过：读不到时前端显示「—」，不显示假的 0。
	unusedMu    sync.Mutex
	unusedCount int
	unusedOK    bool
	unusedAt    time.Time
}

// New 构造 HTTP 服务。
func New(d Deps) *Server {
	s := &Server{
		cfg:  d.Cfg,
		st:   d.Store,
		sc:   d.Scanner,
		up:   d.Updater,
		nt:   d.Notify,
		reg:  d.Registry,
		dk:   d.Docker,
		self: d.Self,
		log:  d.Logger,
	}
	// 环境变量里的凭据只作首次启动的初值，之后以数据库里的设置为准
	s.authUser, s.authPass = d.Cfg.AdminUser, d.Cfg.AdminPassword
	return s
}

// auth 读取当前生效的访问凭据。
func (s *Server) auth() (user, pass string) {
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	return s.authUser, s.authPass
}

// setAuth 更新访问凭据。pass 为空表示关闭登录。
func (s *Server) setAuth(user, pass string) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.authUser, s.authPass = strings.TrimSpace(user), pass
}

// AuthEnabled 是否启用了访问密码。
func (s *Server) AuthEnabled() bool {
	_, pass := s.auth()
	return pass != ""
}

// Handler 组装路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/stats", s.handleStats)

	// 「容器」与「镜像」是两个维度的东西，接口也分开：
	// 容器能重建、镜像只能重拉或删除，混在一个列表里用户分不清点下去动了什么。
	mux.HandleFunc("GET /api/containers", s.handleListContainers)
	mux.HandleFunc("GET /api/images", s.handleListImages)
	mux.HandleFunc("POST /api/images", s.handleAddImage)
	mux.HandleFunc("POST /api/images/pull", s.handlePullImage)
	mux.HandleFunc("POST /api/images/remove", s.handleRemoveLocalImages)
	mux.HandleFunc("DELETE /api/images/{id}", s.handleDeleteImage)
	mux.HandleFunc("PUT /api/images/{id}/ignored", s.handleIgnoreImage)
	mux.HandleFunc("PUT /api/images/{id}/mode", s.handleModeImage)

	mux.HandleFunc("POST /api/scan", s.handleScan)
	mux.HandleFunc("GET /api/scans", s.handleScans)

	mux.HandleFunc("GET /api/notifications", s.handleListNotifications)
	mux.HandleFunc("POST /api/notifications/{id}/read", s.handleReadNotification)
	mux.HandleFunc("POST /api/notifications/read-all", s.handleReadAll)
	mux.HandleFunc("DELETE /api/notifications/read", s.handleClearRead)

	mux.HandleFunc("GET /api/tasks", s.handleListTasks)
	mux.HandleFunc("GET /api/tasks/{id}", s.handleTaskDetail)
	mux.HandleFunc("POST /api/update", s.handleManualUpdate)

	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)

	mux.HandleFunc("POST /api/feishu/test", s.handleFeishuTest)
	// 飞书链路状态：长连接是否就绪、服务启动以来收没收到过按钮回调
	mux.HandleFunc("GET /api/feishu/status", s.handleFeishuStatus)

	mux.HandleFunc("POST /api/telegram/test", s.handleTelegramTest)
	// 只验网络与 Token：不要求 chat_id，用来区分「连不上」和「没配会话」
	mux.HandleFunc("POST /api/telegram/check", s.handleTelegramCheck)
	// Telegram 链路状态：长轮询是否在跑、最近在哪些会话里收到过消息
	mux.HandleFunc("GET /api/telegram/status", s.handleTelegramStatus)

	mux.HandleFunc("POST /api/dingtalk/test", s.handleDingTalkTest)
	// 只验凭据：能不能换到 accessToken，不要求模板与会话
	mux.HandleFunc("POST /api/dingtalk/check", s.handleDingTalkCheck)
	// 钉钉链路状态：Stream 是否在跑、机器人在哪些群里收到过消息
	mux.HandleFunc("GET /api/dingtalk/status", s.handleDingTalkStatus)

	mux.HandleFunc("POST /api/wecom/test", s.handleWeComTest)
	// 企业微信链路状态：长连接是否在线、机器人在哪些会话里收到过消息
	mux.HandleFunc("GET /api/wecom/status", s.handleWeComStatus)

	// 注册表连通性诊断：列表里出现「未知」时用来定位到底是哪一环不通
	mux.HandleFunc("POST /api/registry/test", s.handleRegistryTest)

	// 自检更新：只报告「有没有新版本 + 怎么升」，不会自己动手升级
	mux.HandleFunc("GET /api/selfcheck", s.handleSelfCheck)
	mux.HandleFunc("POST /api/selfcheck", s.handleSelfCheckRun)

	mux.Handle("/", s.staticHandler())

	return s.withLogging(s.withAuth(mux))
}

// staticHandler 优先使用磁盘目录（便于开发时热改），否则回退到编译进二进制的内嵌资源。
//
// 一律禁掉强缓存。内嵌资源没有 mtime，浏览器只能按启发式规则猜缓存时长，
// 于是升级完 fpk 打开页面可能仍跑着上一版的 app.js —— 现象是「改了的界面没生效」，
// 很难往缓存上想。局域网里每次重传几十 KB 不值一提，别为这点流量留隐患。
func (s *Server) staticHandler() http.Handler {
	var h http.Handler
	if dir := s.cfg.StaticDir; dir != "" {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			h = http.FileServer(http.Dir(dir))
		}
	}
	if h == nil {
		sub, err := fs.Sub(web.Files, ".")
		if err != nil {
			s.log.Error("加载内嵌前端资源失败", "err", err)
			return http.NotFoundHandler()
		}
		h = http.FileServer(http.FS(sub))
	}
	return noCache(h)
}

// noCache 给静态资源加上禁止缓存的响应头。
// 必须在 FileServer 之前设置：由 ServeContent 自己写头时不会覆盖已有值。
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// withAuth 在配置了访问密码时启用 HTTP Basic 认证。
// 本服务挂载了 docker.sock，若暴露在公网务必设置密码。
//
// 密码默认为空，也就是**默认不启用登录** —— 装在自家 NAS 上时，
// 每次打开都要求输密码只会惹人烦。需要加一道门时在设置页里填，保存即生效。
//
// 只拦 /api 下的接口，静态页面本身必须能匿名打开：
// 页面被 401 挡住时浏览器会弹出原生登录框，而在飞牛桌面的 iframe 里，
// 这个弹框体验很差，部分壳还会直接判定页面不可用、显示成白屏。
// 所以改由页面自己渲染登录界面，拿到凭据后给每个请求带上 Authorization 头。
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass := s.auth()
		if pass == "" || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if user == "" {
			user = "admin"
		}
		u, p, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="AutoDocker"`)
			writeErr(w, http.StatusUnauthorized, "需要登录")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 前端资源不记日志，避免噪音
		if len(r.URL.Path) >= 4 && r.URL.Path[:4] == "/api" {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			s.log.Debug("http",
				"method", r.Method, "path", r.URL.Path,
				"status", rec.status, "cost", time.Since(start).String())
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- 响应辅助 ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if v == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

func writeOK(w http.ResponseWriter, v any) { writeJSON(w, http.StatusOK, v) }

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func pathID(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("非法的 ID: " + raw)
	}
	return id, nil
}

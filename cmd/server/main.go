package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/haihaipypy/autodocker/internal/api"
	"github.com/haihaipypy/autodocker/internal/config"
	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/notify"
	"github.com/haihaipypy/autodocker/internal/notify/feishu"
	"github.com/haihaipypy/autodocker/internal/registry"
	"github.com/haihaipypy/autodocker/internal/scanner"
	"github.com/haihaipypy/autodocker/internal/selfcheck"
	"github.com/haihaipypy/autodocker/internal/store"
	"github.com/haihaipypy/autodocker/internal/updater"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)

	if dir := filepath.Dir(cfg.DBPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建数据目录失败: %w", err)
		}
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// 上次进程若非正常退出，可能留下永远停在 running 的任务
	if n, rerr := st.RecoverStaleTasks(); rerr == nil && n > 0 {
		logger.Warn("已清理上次异常中断的更新任务", "count", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dk, err := dockerx.New(cfg.DockerHost)
	if err != nil {
		return err
	}
	defer dk.Close()

	pingCtx, cancelPing := context.WithTimeout(ctx, 10*time.Second)
	if perr := dk.Ping(pingCtx); perr != nil {
		logger.Warn("Docker 守护进程暂时不可达，将在扫描时重试", "err", perr)
	} else {
		logger.Info("已连接 Docker 守护进程", "host", cfg.DockerHost)
	}
	cancelPing()

	// 守护进程若已经配好镜像加速器就沿用它。
	//
	// 飞牛上 Docker 拉得动镜像，多半正是靠 daemon.json 里的 registry-mirrors；
	// 而本服务查 digest 是自己发 HTTP 请求，不会继承那份配置，
	// 于是很容易出现「镜像能拉、检测全是未知」。这里主动对齐一次。
	if strings.TrimSpace(cfg.RegistryMirror) == "" {
		mctx, cancelMirror := context.WithTimeout(ctx, 10*time.Second)
		mirrors, merr := dk.RegistryMirrors(mctx)
		cancelMirror()
		if merr == nil && len(mirrors) > 0 {
			cfg.RegistryMirror = strings.Join(mirrors, ",")
			logger.Info("已沿用 Docker 守护进程的镜像加速器", "mirrors", mirrors)
		}
	}

	// 用户没填、守护进程也没配，就走内置默认。
	//
	// 放在这一层兜底而不是写进 config 的默认值里，是为了保住上面「沿用守护进程」
	// 那条路 —— 直接给环境变量塞默认值会让守护进程的配置永远轮不到，属倒退。
	if strings.TrimSpace(cfg.RegistryMirror) == "" {
		cfg.RegistryMirror = registry.DefaultMirror
		logger.Info("未配置镜像加速器，使用内置默认", "mirrors", registry.DefaultMirror)
	}

	reg := registry.New(registry.Options{
		Mirrors:  registry.ParseMirrors(cfg.RegistryMirror),
		Insecure: cfg.RegistryInsecure,
		Auth:     cfg.RegistryAuth,
	})

	fs := feishu.New(feishu.Config{
		AppID:     cfg.FeishuAppID,
		AppSecret: cfg.FeishuAppSecret,
		ChatID:    cfg.FeishuChatID,
	})

	up := updater.New(dk, st, updater.Options{
		PullTimeout:       cfg.PullTimeout,
		StopTimeoutSec:    30,
		HealthCheckWait:   cfg.HealthCheckWait,
		RollbackOnFailure: cfg.RollbackOnFailure,
		KeepOldContainer:  cfg.KeepOldContainer,
		SelfContainer:     cfg.SelfContainer,
		SelfContainerID:   cfg.SelfContainerID,
	}, logger)

	nt := notify.New(ctx, fs, st, up, logger)

	// 自检器只查版本、只给步骤，不碰二进制本身（见 internal/selfcheck 的包说明）
	self := selfcheck.New(logger)

	sc := scanner.New(dk, reg, st, scanner.Options{
		ExtraWatch:      cfg.Watch,
		SelfContainer:   cfg.SelfContainer,
		SelfContainerID: cfg.SelfContainerID,
		Concurrency:     4,
	}, logger)
	sc.SetNotifier(nt)

	srv := api.New(api.Deps{
		Cfg:      cfg,
		Store:    st,
		Scanner:  sc,
		Updater:  up,
		Notify:   nt,
		Registry: reg,
		Docker:   dk,
		Self:     self,
		Logger:   logger,
	})
	// 以持久化设置为准（环境变量只作首次启动的初值）。
	// 飞书凭据也走这里下发，所以长连接在应用了设置之后就已经就绪 ——
	// 用户装完在网页里补填凭据时，保存动作会再次调用同一入口把它拉起来，
	// 不需要重启进程，因此这里不再单独启动长连接。
	settings := srv.LoadSettings()
	srv.ApplySettings(settings)

	if msg := configHints(settings); msg != "" {
		logger.Warn("配置提示：" + msg)
	}
	if !srv.AuthEnabled() {
		logger.Info("未启用访问密码，打开页面即可直接使用；请勿把端口暴露到公网")
	}

	go scheduler(ctx, sc, srv, logger)

	// 自检每天一次，开关读的是设置页里那一份（改了立即生效，不必重启）
	go self.Loop(ctx, func() bool { return srv.LoadSettings().CheckUpdates })

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Info("收到退出信号，正在关闭服务")
		cancel()
		shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	logger.Info("AutoDocker 已启动",
		"port", cfg.Port,
		"scan_interval", cfg.ScanInterval.String(),
		"feishu_ready", fs.Configured(),
		"auth_enabled", cfg.AdminPassword != "")

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	nt.Close()
	return nil
}

// scheduler 周期性触发扫描。用固定的短 tick 轮询「是否到点」，
// 这样在页面修改扫描间隔后无需重启即可生效。
func scheduler(ctx context.Context, sc *scanner.Scanner, srv *api.Server, logger *slog.Logger) {
	go func() {
		if _, err := sc.Scan(ctx, false); err != nil && !errors.Is(err, scanner.ErrScanRunning) {
			logger.Warn("启动扫描失败", "err", err)
		}
	}()

	last := time.Now()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			interval := srv.LoadSettings().ScanInterval
			if interval <= 0 {
				continue
			}
			if time.Since(last) < time.Duration(interval)*time.Second {
				continue
			}
			last = time.Now()
			if _, err := sc.Scan(ctx, false); err != nil && !errors.Is(err, scanner.ErrScanRunning) {
				logger.Warn("定时扫描失败", "err", err)
			}
		}
	}
}

// configHints 汇总常见配置缺失，仅作提示不阻断启动。
//
// 判断依据是「实际生效的设置」而不是环境变量：飞书凭据现在存在数据库里，
// 只看环境变量会在每次重启时误报一遍「未配置飞书」。
func configHints(s model.Settings) string {
	var problems []string
	if s.FeishuAppID == "" || s.FeishuAppSecret == "" {
		problems = append(problems, "未配置飞书应用，发现新版本只能通过网页操作更新")
	}
	if s.FeishuChatID == "" {
		problems = append(problems, "未配置飞书接收群，通知无法推送")
	}
	return strings.Join(problems, "；")
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv})
	return slog.New(handler)
}

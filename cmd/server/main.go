package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/haihaipypy/autodocker/internal/api"
	"github.com/haihaipypy/autodocker/internal/config"
	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/hub"
	"github.com/haihaipypy/autodocker/internal/hubproto"
	"github.com/haihaipypy/autodocker/internal/hubserver"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/notify"
	"github.com/haihaipypy/autodocker/internal/notify/feishu"
	"github.com/haihaipypy/autodocker/internal/registry"
	"github.com/haihaipypy/autodocker/internal/scanner"
	"github.com/haihaipypy/autodocker/internal/selfcheck"
	"github.com/haihaipypy/autodocker/internal/selfupgrade"
	"github.com/haihaipypy/autodocker/internal/store"
	"github.com/haihaipypy/autodocker/internal/updater"
)

func main() {
	// 升级 helper 必须在加载配置之前分流：它跑在一个临时容器里，没有环境变量、
	// 没有数据卷以外的东西，唯一要做的就是把旧容器换成新版或者原样退回去。
	if len(os.Args) > 1 && os.Args[1] == selfupgrade.HelperCommand {
		if err := selfupgrade.RunHelper(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "升级失败:", err)
			os.Exit(1)
		}
		return
	}
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

	// Agent 模式下把日志同时接进环形缓冲，随每次上报送到 Hub。
	// 目标机通常没有 SSH，出问题时唯一的线索就是这几十行。
	//
	// 注意这里是「攒一个 writer 列表再展开」，而不是直接传 *hub.Ring 的 nil：
	// 一个 nil 的具体类型指针装进 io.Writer 接口后并不等于 nil，
	// MultiWriter 会把它当有效 writer 调下去，然后在 Write 里空指针崩溃。
	var logRing *hub.Ring
	var logWriters []io.Writer
	if cfg.Mode == config.ModeAgent {
		logRing = hub.NewRing(200)
		logWriters = append(logWriters, logRing)
	}
	logger := newLogger(cfg.LogLevel, logWriters...)

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

	alignRegistryMirror(ctx, cfg, dk, logger)

	reg := registry.New(registry.Options{
		Mirrors:  registry.ParseMirrors(cfg.RegistryMirror),
		Insecure: cfg.RegistryInsecure,
		Auth:     cfg.RegistryAuth,
	})

	// 被监控端到此为止：往下都是网页、通知、自检，Agent 一个都不要。
	if cfg.Mode == config.ModeAgent {
		return runAgent(ctx, cfg, logger, st, dk, reg, logRing)
	}

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

	// 远程节点接入点。Hub 只收上报、下发指令，判定逻辑在 Agent 那边。
	//
	// 两个回调把三块能力串起来：
	//   - Notifier：远端发现更新时由 Hub 统一发提醒（渠道凭据只在 Hub 上）；
	//   - OnOp：Agent 报回的进度用来把卡片从「更新中」翻成结果。
	//
	// 自动升级要读设置页里的开关，而设置属于 api.Server —— 它是后面才建的。
	// 用一个原子指针把「后建的 srv」接进来，比在这里复制一份设置初值可靠：
	// 用户在页面上关掉开关后，下一次节点连上来立刻就能读到新值。
	var settingsOwner atomic.Pointer[api.Server]

	hubsrv := hubserver.New(hubserver.Config{
		Version:      selfcheck.Version,
		ScanInterval: int(cfg.ScanInterval / time.Second),
		Notifier:     nt,
		OnOp:         nt.NodeOpEvent,
		AgentImage:   selfcheck.AgentImage(cfg.AgentImage),
		AutoUpgrade: func() bool {
			srv := settingsOwner.Load()
			return srv != nil && srv.AgentAutoUpgrade()
		},
	}, st, logger)
	// 反过来把节点通道交给通知服务：卡片上点「更新」时才知道往哪台机器发指令。
	nt.SetRemoteNodes(hubsrv)

	srv := api.New(api.Deps{
		Cfg:      cfg,
		Store:    st,
		Scanner:  sc,
		Updater:  up,
		Notify:   nt,
		Registry: reg,
		Docker:   dk,
		Self:     self,
		Hub:      hubsrv,
		Logger:   logger,
	})
	settingsOwner.Store(srv)
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
		"version", selfcheck.Version,
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

// alignRegistryMirror 让「查 digest 用的加速器」与守护进程保持一致。
//
// 飞牛上 Docker 拉得动镜像，多半正是靠 daemon.json 里的 registry-mirrors；
// 而本服务查 digest 是自己发 HTTP 请求，不会继承那份配置，
// 于是很容易出现「镜像能拉、检测全是未知」。这里主动对齐一次。
//
// 放在这一层兜底而不是写进 config 的默认值里，是为了保住「沿用守护进程」
// 那条路 —— 直接给环境变量塞默认值会让守护进程的配置永远轮不到，属倒退。
func alignRegistryMirror(ctx context.Context, cfg *config.Config, dk *dockerx.Client, logger *slog.Logger) {
	if strings.TrimSpace(cfg.RegistryMirror) == "" {
		mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		mirrors, merr := dk.RegistryMirrors(mctx)
		cancel()
		if merr == nil && len(mirrors) > 0 {
			cfg.RegistryMirror = strings.Join(mirrors, ",")
			logger.Info("已沿用 Docker 守护进程的镜像加速器", "mirrors", mirrors)
		}
	}
	if strings.TrimSpace(cfg.RegistryMirror) == "" {
		cfg.RegistryMirror = registry.DefaultMirror
		logger.Info("未配置镜像加速器，使用内置默认", "mirrors", registry.DefaultMirror)
	}
}

// runAgent 是被监控端的全部逻辑：巡检 → 上报 → 等指令。
//
// 这里刻意不建 HTTP 服务、不建通知渠道、不跑自检 ——
// Agent 不开任何端口，通知统一由 Hub 发。目标机上它等于一个不存在的东西。
func runAgent(ctx context.Context, cfg *config.Config, logger *slog.Logger,
	st *store.Store, dk *dockerx.Client, reg *registry.Client, ring *hub.Ring) error {

	actx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	sc := scanner.New(dk, reg, st, scanner.Options{
		ExtraWatch:      cfg.Watch,
		SelfContainer:   cfg.SelfContainer,
		SelfContainerID: cfg.SelfContainerID,
		Concurrency:     4,
		// Agent 只上报容器：镜像维度在这台机器上没有任何展示入口，
		// 而它的检测是串行的，访问不了 Docker Hub 的机器上能耗掉几十分钟。
		SkipImages: true,
		// 自升级用的 helper 与「改名待删」的旧容器不是用户的容器，
		// 出现在 Hub 的节点列表里只会让人困惑（还带着「可选更新」）。
		ExcludeContainers: selfupgrade.AuxContainers(cfg.SelfContainer),
	}, logger)
	// 刻意不注入 notifier：Agent 只上报，通知统一由 Hub 发。
	// scanner.notify() 在 notifier 为 nil 时只落库不推送，正好是这个语义。
	//
	// 但 updater 要建：1.8.0 起 Hub 可以下发「重建这个容器」，
	// 判定（要不要更新、更新到哪个 tag）仍留在 Agent 这边，Hub 只说重建谁。
	up := updater.New(dk, st, updater.Options{
		PullTimeout:       cfg.PullTimeout,
		StopTimeoutSec:    30,
		HealthCheckWait:   cfg.HealthCheckWait,
		RollbackOnFailure: cfg.RollbackOnFailure,
		KeepOldContainer:  cfg.KeepOldContainer,
		SelfContainer:     cfg.SelfContainer,
		SelfContainerID:   cfg.SelfContainerID,
	}, logger)

	// 上一轮自动升级如果发生过，结局记在数据卷里。这里读一次就删，
	// 随首份上报带给 Hub —— 用户能在节点页的日志里看到成败与原因。
	selfupgrade.Cleanup(actx, dk, logger)
	upgradeRes := selfupgrade.ConsumeResult()
	if upgradeRes != nil {
		if upgradeRes.OK {
			logger.Info("上次自动升级成功", "from", upgradeRes.From, "to", upgradeRes.Target)
		} else {
			logger.Warn("上次自动升级失败并已回滚", "from", upgradeRes.From,
				"to", upgradeRes.Target, "err", upgradeRes.Error)
		}
	}

	hc := hub.New(hub.Config{
		HubURL:          cfg.HubURL,
		Token:           cfg.NodeToken,
		NodeName:        cfg.NodeName,
		Version:         selfcheck.Version,
		ScanInterval:    cfg.ScanInterval,
		SelfContainer:   cfg.SelfContainer,
		SelfContainerID: cfg.SelfContainerID,
	}, sc, st, dk, up, logger, ring)
	if upgradeRes != nil {
		hc.SetUpgradeResult(&hubproto.UpgradeResult{
			OK:     upgradeRes.OK,
			From:   upgradeRes.From,
			Target: upgradeRes.Target,
			Error:  upgradeRes.Error,
			At:     upgradeRes.FinishedAt,
		})
	}

	logger.Info("AutoDocker Agent 已启动",
		"version", selfcheck.Version,
		"hub", cfg.HubURL,
		"node", cfg.NodeName,
		"scan_interval", cfg.ScanInterval.String())

	return hc.Run(actx)
}

func newLogger(level string, extra ...io.Writer) *slog.Logger {
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

	var w io.Writer = os.Stdout
	if len(extra) > 0 {
		writers := []io.Writer{os.Stdout}
		for _, e := range extra {
			if e != nil {
				writers = append(writers, e)
			}
		}
		if len(writers) > 1 {
			w = io.MultiWriter(writers...)
		}
	}

	handler := slog.NewTextHandler(w, &slog.HandlerOptions{Level: lv})
	return slog.New(handler)
}

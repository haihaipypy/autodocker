// Package updater 执行容器更新：拉取新镜像并按原配置重建容器。
//
// 这是整个项目风险最高的环节。Docker 没有「原地更新容器」的能力，
// 只能 inspect 出原配置 → 停旧 → 建新 → 起新 → 健康检查。
// 因此这里的原则是：**配置零丢失 + 全程可回滚**。
//
// 实现上刻意把 Config / HostConfig 当作原始 JSON 原样搬运：
// 不做字段级的类型映射，就不会因为 Docker API 新增字段而悄悄丢配置。
package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/store"
	"github.com/haihaipypy/autodocker/internal/version"
)

// Options 更新行为配置。
type Options struct {
	PullTimeout       time.Duration
	StopTimeoutSec    int
	HealthCheckWait   time.Duration
	RollbackOnFailure bool
	KeepOldContainer  bool
	SelfContainer     string
	SelfContainerID   string
}

// Request 一次更新请求。
type Request struct {
	TaskID        int64
	ImageRef      string
	ContainerID   string
	ContainerName string
	TriggeredBy   string
}

// PullRequest 一次「只拉镜像」的请求。
type PullRequest struct {
	TaskID   int64
	ImageRef string
}

// Updater 更新执行器。同一时刻只允许一个更新在跑，避免并发重建互相踩踏。
type Updater struct {
	dk  *dockerx.Client
	st  *store.Store
	opt Options
	log *slog.Logger

	mu      sync.Mutex
	running bool
}

// New 构造更新执行器。
func New(dk *dockerx.Client, st *store.Store, opt Options, logger *slog.Logger) *Updater {
	opt = withDefaults(opt)
	return &Updater{dk: dk, st: st, opt: opt, log: logger}
}

func withDefaults(opt Options) Options {
	if opt.PullTimeout <= 0 {
		opt.PullTimeout = 30 * time.Minute
	}
	if opt.StopTimeoutSec <= 0 {
		opt.StopTimeoutSec = 30
	}
	if opt.SelfContainer == "" {
		opt.SelfContainer = "autodocker"
	}
	return opt
}

// SetOptions 热更新配置。
func (u *Updater) SetOptions(opt Options) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.opt = withDefaults(opt)
}

// Busy 是否有更新正在执行。
func (u *Updater) Busy() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.running
}

// Run 执行一次容器更新，阻塞至结束。
// 返回非空 error 表示更新未成功（可能已回滚）。
func (u *Updater) Run(ctx context.Context, req Request) error {
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		err := errors.New("已有更新任务正在执行，请稍后重试")
		_ = u.st.FinishTask(req.TaskID, model.TaskFailed, "", err.Error())
		return err
	}
	u.running = true
	opt := u.opt
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.running = false
		u.mu.Unlock()
	}()

	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		u.log.Info(line, "task_id", req.TaskID, "container", req.ContainerName)
		if err := u.st.AppendTaskLog(req.TaskID, "["+time.Now().Format("15:04:05")+"] "+line); err != nil {
			u.log.Warn("写入任务日志失败", "err", err)
		}
	}
	fail := func(err error) error {
		_ = u.st.FinishTask(req.TaskID, model.TaskFailed, "", err.Error())
		return err
	}

	// 自我保护：绝不能更新自己，否则进程被杀、任务中断
	if req.ContainerName == opt.SelfContainer {
		logf("已阻止：目标 %s 是本服务自身", req.ContainerName)
		return fail(errors.New("拒绝更新自身容器"))
	}

	// 拿真正要动的容器。库里记的编号可能已经失效，按名字也能找回来。
	insp, cid, err := u.resolveContainer(ctx, req)
	if err != nil {
		return fail(err)
	}
	if opt.SelfContainerID != "" && strings.HasPrefix(cid, opt.SelfContainerID) {
		logf("已阻止：目标 %s 是本服务自身", req.ContainerName)
		return fail(errors.New("拒绝更新自身容器"))
	}
	if len(insp.Config) == 0 || len(insp.HostConfig) == 0 {
		return fail(errors.New("容器配置不完整，无法安全重建"))
	}

	origName := strings.TrimPrefix(insp.Name, "/")
	if origName == "" {
		origName = req.ContainerName
	}

	if cid != req.ContainerID {
		// 顺手把库里那一行挂回真容器，否则下一次点更新还是同一个 404。
		if img, ferr := u.st.FindImageByContainer(req.ContainerID); ferr == nil {
			if uerr := u.st.UpdateImageContainer(img.ID, cid, origName); uerr != nil {
				u.log.Warn("修正监控项容器编号失败", "id", img.ID, "err", uerr)
			}
		}
		req.ContainerID = cid
	}

	wasRunning := insp.State != nil && insp.State.Running
	stateText := ""
	if insp.State != nil {
		stateText = insp.State.Status
	}
	backupName := fmt.Sprintf("%s-old-%s", origName, time.Now().Format("20060102-150405"))

	// 自动删除的容器一旦停止就会被 Docker 立即回收，无法保留回滚现场
	if autoRemove, _ := insp.HostConfig["AutoRemove"].(bool); autoRemove {
		return fail(errors.New("目标容器启用了自动删除（--rm），停止后会被立即回收、无法回滚，已拒绝更新"))
	}

	if err := u.st.StartTask(req.TaskID, backupName); err != nil {
		u.log.Warn("更新任务状态失败", "err", err)
	}
	logf("开始更新容器 %s（镜像 %s，原状态 %s）", origName, req.ImageRef, stateText)

	// 先把重建所需配置准备好，任何问题都在动手之前暴露
	cfg, hostCfg, netCfg, warns := buildSpec(insp, req.ImageRef)
	for _, w := range warns {
		logf("注意：%s", w)
	}

	// 拉取新镜像
	logf("拉取镜像 %s", req.ImageRef)
	pullCtx, cancelPull := context.WithTimeout(ctx, opt.PullTimeout)
	err = u.dk.PullImage(pullCtx, req.ImageRef, func(format string, args ...any) {
		logf(format, args...)
	})
	cancelPull()
	if err != nil {
		return fail(err)
	}
	logf("镜像拉取完成")

	// 容器已经在跑目标镜像：重建只是白停一次服务、白断一次连接。
	// 正常路径走不到这里，只有「上次重建成功但状态没写回」这类历史数据会撞上 ——
	// 那时点更新，容器其实早就是新版了。比的是镜像编号，不是引用字符串：
	// 「同一个 tag 内容变了」正是这个工具要抓的场景，只比引用会把真更新误判成已最新。
	if sameImageID(insp.Image, u.imageIDForRef(ctx, req.ImageRef)) {
		logf("容器已运行在 %s 上，无需重建，只刷新监控项", req.ImageRef)
		u.refreshMonitor(ctx, req, req.ContainerID, origName)
		_ = u.st.FinishTask(req.TaskID, model.TaskSuccess, req.ContainerID, "")
		return nil
	}

	// 停旧容器，腾出容器名与端口
	if wasRunning {
		logf("停止旧容器")
		if err := u.dk.StopContainer(ctx, req.ContainerID, opt.StopTimeoutSec); err != nil {
			return fail(err)
		}
	}

	// 重命名旧容器：既腾出原名，也保留回滚现场
	if err := u.dk.RenameContainer(ctx, req.ContainerID, backupName); err != nil {
		if wasRunning {
			_ = u.dk.StartContainer(ctx, req.ContainerID)
		}
		return fail(err)
	}
	logf("旧容器已重命名为 %s", backupName)

	// 用原配置创建新容器
	newID, err := u.dk.CreateContainer(ctx, cfg, hostCfg, netCfg, origName)
	if err != nil {
		logf("创建新容器失败，开始回滚")
		u.rollback(ctx, "", req.ContainerID, origName, wasRunning, logf)
		return fail(err)
	}
	logf("新容器已创建 %s", shortID(newID))

	if err := u.dk.StartContainer(ctx, newID); err != nil {
		logf("启动新容器失败，开始回滚")
		u.rollback(ctx, newID, req.ContainerID, origName, wasRunning, logf)
		return fail(err)
	}

	// 健康检查：确认新容器真的活了再清理旧容器
	if wasRunning {
		ok, msg := u.waitHealthy(ctx, newID, opt.HealthCheckWait, logf)
		if !ok {
			if opt.RollbackOnFailure {
				logf("健康检查未通过（%s），执行回滚", msg)
				u.rollback(ctx, newID, req.ContainerID, origName, wasRunning, logf)
				_ = u.st.FinishTask(req.TaskID, model.TaskRolledBack, "", msg)
				return errors.New(msg)
			}
			logf("健康检查未通过（%s），但已关闭自动回滚，保留新容器", msg)
			_ = u.st.FinishTask(req.TaskID, model.TaskFailed, newID, msg)
			return errors.New(msg)
		}
		logf("健康检查通过：%s", msg)
	}

	// 更新成功
	if opt.KeepOldContainer {
		logf("保留旧容器 %s（按配置未删除）", backupName)
	} else {
		logf("清理旧容器 %s", backupName)
		if err := u.dk.RemoveContainer(ctx, req.ContainerID, false, false); err != nil {
			logf("警告：删除旧容器失败（%v），可手动清理 %s", err, backupName)
		}
	}
	logf("更新完成：%s 已运行在新镜像上", origName)

	// 刷新监控项：容器号变了，引用也可能变（v3.6.0 → v3.7.0 这类升 tag 的重建）。
	u.refreshMonitor(ctx, req, newID, origName)

	_ = u.st.FinishTask(req.TaskID, model.TaskSuccess, newID, "")
	return nil
}

// resolveContainer 找到这次真正要更新的容器，返回详情与它当前的编号。
//
// 库里记的容器编号会过期：重建成功却没把新编号写回（1.5.7 之前的表现），
// 或者容器被删掉后重新建了一个同名的。编号每次 create 都变，容器名在重建时
// 是刻意保留的 —— 所以编号查不到时按名字找回来。
// 少了这一步，那条记录会永久卡在「更新失败：No such container」，
// 用户唯一的出路是删掉监控项重加，历史一起丢。
func (u *Updater) resolveContainer(ctx context.Context, req Request) (*dockerx.Inspect, string, error) {
	insp, err := u.dk.InspectContainer(ctx, req.ContainerID)
	if err == nil {
		return insp, req.ContainerID, nil
	}
	if !dockerx.IsNotFound(err) || req.ContainerName == "" {
		return nil, "", err
	}

	containers, lerr := u.dk.ListContainers(ctx)
	if lerr != nil {
		return nil, "", err
	}
	want := strings.TrimPrefix(req.ContainerName, "/")
	for _, c := range containers {
		if c.Name != want {
			continue
		}
		found, ierr := u.dk.InspectContainer(ctx, c.ID)
		if ierr != nil {
			continue
		}
		u.log.Warn("监控项记录的容器编号已失效，按容器名找回",
			"container", want, "stale_id", shortID(req.ContainerID), "current_id", shortID(c.ID))
		return found, c.ID, nil
	}

	return nil, "", fmt.Errorf("容器 %s 已不存在（记录里的编号 %s 也查不到），可能已被删除；请在容器列表里重新添加监控",
		want, shortID(req.ContainerID))
}

// sameImageID 两个镜像编号是否指向同一份内容。
func sameImageID(a, b string) bool {
	return a != "" && b != "" &&
		strings.TrimPrefix(a, "sha256:") == strings.TrimPrefix(b, "sha256:")
}

// normalizeRef 把镜像引用归一成可比较的形式：去掉 Docker Hub 的几种写法差异，补齐默认 tag。
func normalizeRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.Contains(ref, "@") {
		return ref
	}
	ref = strings.TrimPrefix(ref, "docker.io/library/")
	ref = strings.TrimPrefix(ref, "docker.io/")
	ref = strings.TrimPrefix(ref, "index.docker.io/")
	ref = strings.TrimPrefix(ref, "registry-1.docker.io/")
	ref = strings.TrimPrefix(ref, "library/")
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon <= slash {
		ref += ":latest"
	}
	return ref
}

// imageIDForRef 本机这个引用当前指向的镜像编号，找不到返回空串。
func (u *Updater) imageIDForRef(ctx context.Context, ref string) string {
	images, err := u.dk.ListImages(ctx)
	if err != nil {
		return ""
	}
	want := normalizeRef(ref)
	for _, img := range images {
		for _, t := range img.Tags {
			if normalizeRef(t) == want {
				return img.ID
			}
		}
	}
	return ""
}

// rebuildStatus 重建完成后这条监控项该是什么状态。
//
// 关键在「别替用户把真实存在的更新藏起来」：只重拉了老 tag 的内容（升 tag 没成功、
// 或者调用的还是旧目标）时，仓库里那个更高的版本仍然在，状态必须留在「可更新」。
// 无脑写「已最新」的后果是页面安静一小时，下一轮扫描又翻出来 —— 看起来像在抽风。
// 用 Compare 而不是 SameVersion：3.7 与 v3.7.0 是同一个版本，
// SameVersion 会因为数字段个数不同判成不一样，于是刚升完就被标回「可更新」。
func rebuildStatus(tag, latestTag string) (model.Status, string) {
	if latestTag != "" && version.Compare(tag, latestTag) < 0 {
		return model.StatusNewTag, latestTag
	}
	return model.StatusUpToDate, ""
}

// refreshMonitor 把监控项对齐到刚重建出来的新容器。
//
// 三件事必须一起做，少一件页面就会说谎：
//   - container_id 换成新容器，否则下一轮扫描按旧号找不到它，会多插一条重复记录；
//   - reference / local_tag 换成这次重建实际用的引用 —— 升 tag 的重建里引用是变的，
//     只改 local_tag 不改 reference，页面上的「当前」也还是旧版本；
//   - 状态按新 tag 重新判定：升到了仓库里那个更高版本才算「已最新」，
//     只重拉了老 tag 的内容就还得继续显示「可更新」。
//
// 查找一律先按容器号：按 (引用, 容器号) 精确查在升 tag 之后必然扑空，
// 而扑空的后果是整段刷新被跳过 —— 卡片显示「更新成功」，页面却停在旧版本 +「可更新」。
func (u *Updater) refreshMonitor(ctx context.Context, req Request, newID, name string) {
	tag := tagOf(req.ImageRef)

	img, err := u.st.FindImageByContainer(req.ContainerID)
	if err != nil {
		alt, aerr := u.st.FindImage(req.ImageRef, req.ContainerID)
		if aerr != nil {
			u.log.Warn("重建成功，但找不到对应的监控项，状态未能刷新",
				"container", req.ContainerName, "container_id", req.ContainerID,
				"ref", req.ImageRef, "err", aerr)
			return
		}
		img = alt
	}

	imageID := ""
	localDigest := img.LocalDigest
	if insp, ierr := u.dk.InspectContainer(ctx, newID); ierr == nil {
		imageID = insp.Image
		if _, d, derr := u.dk.DigestForImage(ctx, insp.Image, req.ImageRef); derr == nil && d != "" {
			localDigest = d
		}
	}

	status, latest := rebuildStatus(tag, img.LatestTag)

	if err := u.st.UpdateImageAfterRebuild(img.ID, req.ImageRef, imageID, newID, name, localDigest, tag); err != nil {
		u.log.Warn("刷新监控项失败", "id", img.ID, "ref", req.ImageRef, "err", err)
		return
	}
	if err := u.st.UpdateImageCheck(img.ID, status, localDigest, latest, img.Aliases, img.Mode, ""); err != nil {
		u.log.Warn("刷新检测状态失败", "id", img.ID, "err", err)
		return
	}
	u.log.Info("已刷新监控项", "container", name, "ref", req.ImageRef,
		"tag", tag, "status", string(status), "latest_tag", latest)
}

// Pull 只把镜像拉到本地，不碰任何容器。
//
// 用于「本机有这个镜像、但没有任何容器在用它」的情形：这里要的只是把本地
// 那份换成新的，没有容器需要重启，也就没有回滚一说。
// 它与 Run 共用同一把互斥锁 —— 让拉取和重建错开，免得两边同时折腾同一批镜像层。
func (u *Updater) Pull(ctx context.Context, req PullRequest) error {
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		err := errors.New("已有更新任务正在执行，请稍后重试")
		_ = u.st.FinishTask(req.TaskID, model.TaskFailed, "", err.Error())
		return err
	}
	u.running = true
	opt := u.opt
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.running = false
		u.mu.Unlock()
	}()

	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		u.log.Info(line, "task_id", req.TaskID, "image", req.ImageRef)
		if err := u.st.AppendTaskLog(req.TaskID, "["+time.Now().Format("15:04:05")+"] "+line); err != nil {
			u.log.Warn("写入任务日志失败", "err", err)
		}
	}

	if err := u.st.StartTask(req.TaskID, ""); err != nil {
		u.log.Warn("更新任务状态失败", "err", err)
	}
	logf("开始拉取镜像 %s", req.ImageRef)

	pullCtx, cancel := context.WithTimeout(ctx, opt.PullTimeout)
	err := u.dk.PullImage(pullCtx, req.ImageRef, func(format string, args ...any) {
		logf(format, args...)
	})
	cancel()
	if err != nil {
		_ = u.st.FinishTask(req.TaskID, model.TaskFailed, "", err.Error())
		return err
	}
	logf("镜像拉取完成")

	// 拉取之后本地那份已经换掉了：镜像 ID、大小、摘要都得重新采一遍，
	// 否则页面上还挂着旧摘要，状态会一直显示成「有更新」。
	if img, ferr := u.st.FindImage(req.ImageRef, ""); ferr == nil {
		fresh := *img
		if list, lerr := u.dk.ListImages(ctx); lerr == nil {
			for _, it := range list {
				matched := false
				for _, tag := range it.Tags {
					if tag == req.ImageRef {
						matched = true
						break
					}
				}
				if matched {
					fresh.ImageID = it.ID
					fresh.SizeBytes = it.Size
					fresh.ImageCreated = it.Created
					break
				}
			}
		}
		if _, d, derr := u.dk.DigestForImage(ctx, fresh.ImageID, req.ImageRef); derr == nil {
			fresh.LocalDigest = d
		}
		fresh.Status = model.StatusUpToDate
		fresh.LastError = ""
		fresh.LatestTag = ""
		fresh.Aliases = nil
		if _, uerr := u.st.UpsertImage(&fresh); uerr != nil {
			u.log.Warn("刷新镜像记录失败", "err", uerr)
		}
	}

	_ = u.st.FinishTask(req.TaskID, model.TaskSuccess, "", "")
	return nil
}

// rollback 把现场恢复到更新之前：删掉新容器、旧容器改回原名并（必要时）重新启动。
func (u *Updater) rollback(ctx context.Context, newID, oldID, origName string, wasRunning bool, logf func(string, ...any)) {
	if newID != "" {
		logf("回滚：删除新容器 %s", shortID(newID))
		if err := u.dk.RemoveContainer(ctx, newID, true, false); err != nil {
			logf("回滚：删除新容器失败（%v）", err)
		}
	}
	logf("回滚：旧容器恢复原名 %s", origName)
	if err := u.dk.RenameContainer(ctx, oldID, origName); err != nil {
		logf("回滚：恢复容器名失败（%v），旧容器仍为备份名，请手动处理", err)
	}
	if wasRunning {
		logf("回滚：重新启动旧容器")
		if err := u.dk.StartContainer(ctx, oldID); err != nil {
			logf("回滚：重新启动旧容器失败（%v），请手动启动", err)
		}
	}
}

// waitHealthy 在新容器启动后做一段健康确认。
//
// 判断依据：
//   - 容器退出 → 立即失败；
//   - 定义了 HEALTHCHECK 且变为 healthy → 通过；变为 unhealthy → 失败；
//   - 未定义 HEALTHCHECK → 在观察窗口内持续运行即视为通过。
func (u *Updater) waitHealthy(ctx context.Context, id string, window time.Duration, logf func(string, ...any)) (bool, string) {
	if window <= 0 {
		window = 15 * time.Second
	}
	deadline := time.Now().Add(window)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		running, health, status, err := u.dk.State(ctx, id)
		if err != nil {
			return false, "读取新容器状态失败: " + err.Error()
		}
		switch {
		case !running:
			return false, "新容器已退出（状态 " + status + "）"
		case health == "healthy":
			return true, "健康检查状态 healthy"
		case health == "unhealthy":
			return false, "健康检查状态 unhealthy"
		}
		if time.Now().After(deadline) {
			if health == "starting" {
				return false, "健康检查在观察窗口内未完成（仍为 starting）"
			}
			return true, "容器保持运行（未定义健康检查或已通过观察）"
		}
		select {
		case <-ctx.Done():
			return false, "等待健康检查被取消"
		case <-ticker.C:
		}
	}
}

// buildSpec 从 inspect 结果还原出重建容器所需的全部配置。
//
// 关键动作有三个：
//  1. Config 与 HostConfig 整份克隆（原样透传，不丢字段），再逐条修正少数「不能照抄」的字段；
//  2. 补上匿名卷这处 inspect 才有、而创建时必须显式给出的信息；
//  3. 只把用户定义网络的接入设置搬回去，内置网络交给 NetworkMode 决定。
//
// targetRef 是本次要更新到的镜像引用。必须显式写入 Config.Image：
// inspect 里的 Config.Image 是「容器创建时用的引用」，在「更高版本 tag」场景下
// 它仍是旧 tag，照抄会重建出一个依旧跑着旧镜像的容器（表面成功、实际什么都没更新）。
func buildSpec(insp *dockerx.Inspect, targetRef string) (cfg, hostCfg, netCfg map[string]any, warns []string) {
	cfg = cloneMap(insp.Config)
	hostCfg = cloneMap(insp.HostConfig)

	if targetRef != "" {
		cfg["Image"] = targetRef
	}

	// Hostname 默认被 Docker 填成容器短 ID。照抄会把「旧容器的 ID」带进新容器，
	// 既没有意义，又会让依赖 hostname 判断的日志/探针产生误导。
	// 只在它确实等于旧容器 ID 前缀（即自动分配）时才清掉；用户手写的 --hostname 保持不动。
	if h, _ := cfg["Hostname"].(string); h != "" && strings.HasPrefix(insp.ID, h) {
		delete(cfg, "Hostname")
	}

	// 匿名卷补齐：容器启动时由 VOLUME 指令隐式创建的卷不会出现在 Binds 里，
	// 若不显式指回原卷名，重建后会挂到一个全新的空卷上，数据即丢失。
	binds := toStringSlice(hostCfg["Binds"])
	binds = appendAnonymousVolumes(binds, toAnySlice(hostCfg["Mounts"]), insp.Mounts)
	if len(binds) > 0 {
		hostCfg["Binds"] = binds
	}

	if v := toStringSlice(hostCfg["VolumesFrom"]); len(v) > 0 {
		warns = append(warns, fmt.Sprintf("该容器使用 --volumes-from %v，请确认被引用容器仍然存在", v))
	}
	if v := toStringSlice(hostCfg["Links"]); len(v) > 0 {
		warns = append(warns, fmt.Sprintf("该容器使用了旧式 --link %v，重建后别名可能失效", v))
	}
	if insp.State != nil && insp.State.Running {
		if mode, _ := hostCfg["NetworkMode"].(string); mode == "host" {
			warns = append(warns, "该容器使用 host 网络，重建期间端口会短暂不可用")
		}
	}

	netCfg = buildNetworkingConfig(insp)
	return cfg, hostCfg, netCfg, warns
}

// buildNetworkingConfig 构造创建容器时需要的 NetworkingConfig。
//
// 这里只搬「用户定义网络」上的接入设置，并且只搬接入真正用得上的字段。两处必须剔除：
//
//   - **内置网络**（bridge / host / none / nat）：容器接哪张网已经由
//     HostConfig.NetworkMode 决定了，再往 EndpointsConfig 里塞一份会被守护进程直接拒绝
//     （container cannot be connected to network endpoints: host）。
//     这也是「用了 host 网络的容器更新失败」最常见的成因。
//
//   - **NetworkID / EndpointID / Gateway / IPAddress / MacAddress**：这些是运行时填充的，
//     原样搬回去会被 Docker 当成「用户指定了 IP/MAC」，在旧容器还占着地址时报冲突。
func buildNetworkingConfig(insp *dockerx.Inspect) map[string]any {
	// 共享其它容器网络栈时，接哪张网由被共享的容器决定，这里一个端点都不能传
	mode, _ := insp.HostConfig["NetworkMode"].(string)
	if strings.HasPrefix(mode, "container:") {
		return nil
	}
	if insp.NetworkSettings == nil || len(insp.NetworkSettings.Networks) == 0 {
		return nil
	}

	eps := map[string]any{}
	for name, ep := range insp.NetworkSettings.Networks {
		if ep == nil || isBuiltinNetwork(name) {
			continue
		}
		entry := map[string]any{}
		if ep.IPAMConfig != nil &&
			(ep.IPAMConfig.IPv4Address != "" || ep.IPAMConfig.IPv6Address != "" || len(ep.IPAMConfig.LinkLocalIPs) > 0) {
			entry["IPAMConfig"] = ep.IPAMConfig
		}
		if len(ep.Links) > 0 {
			entry["Links"] = ep.Links
		}
		if len(ep.Aliases) > 0 {
			entry["Aliases"] = ep.Aliases
		}
		if len(ep.DriverOpts) > 0 {
			entry["DriverOpts"] = ep.DriverOpts
		}
		// 即使没有任何自定义设置也要保留这个端点：
		// 接在多张网上的容器需要显式列出要接入的每一张网络。
		eps[name] = entry
	}
	if len(eps) == 0 {
		return nil
	}
	return map[string]any{"EndpointsConfig": eps}
}

// isBuiltinNetwork 判断是否 Docker 自带网络。
func isBuiltinNetwork(name string) bool {
	switch name {
	case "bridge", "host", "none", "nat", "default":
		return true
	}
	return false
}

// appendAnonymousVolumes 把隐式卷补成显式绑定，避免重建时数据落到新空卷。
func appendAnonymousVolumes(binds []string, declared []any, actual []dockerx.MountPoint) []string {
	covered := map[string]struct{}{}
	for _, b := range binds {
		if dest := bindDestination(b); dest != "" {
			covered[dest] = struct{}{}
		}
	}
	for _, m := range declared {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if target, ok := mm["Target"].(string); ok && target != "" {
			covered[target] = struct{}{}
		}
	}

	out := make([]string, 0, len(binds)+len(actual))
	out = append(out, binds...)
	for _, mp := range actual {
		if mp.Type != "volume" || mp.Name == "" || mp.Destination == "" {
			continue
		}
		if _, ok := covered[mp.Destination]; ok {
			continue
		}
		out = append(out, mp.Name+":"+mp.Destination)
	}
	return out
}

// mountModes 是 -v 表达式末尾允许出现的挂载选项。
var mountModes = map[string]struct{}{
	"ro": {}, "rw": {}, "z": {}, "Z": {}, "nocopy": {},
	"cached": {}, "delegated": {}, "consistent": {},
	"private": {}, "rprivate": {}, "shared": {}, "rshared": {},
	"slave": {}, "rslave": {}, "unbindable": {}, "runbindable": {},
}

// bindDestination 取出 -v 表达式里的「容器内路径」。
//
// 需要同时应付 `vol:/data`、`vol:/data:ro`、`/data`，
// 以及 Windows 上可能出现的带盘符源路径 `C:\dir:/data`，
// 所以不能简单地按冒号切完固定取第二段——否则覆盖判断会漏，
// 补出一个重复的挂载点，创建阶段会直接报 duplicate mount point。
func bindDestination(b string) string {
	parts := strings.Split(b, ":")
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0] // 只有容器内路径
	case 2:
		return parts[1] // 源:目标
	}
	// 三段及以上：末段可能是挂载选项
	if isMountMode(parts[len(parts)-1]) {
		return parts[len(parts)-2]
	}
	return parts[len(parts)-1]
}

// isMountMode 判断一段是否是挂载选项（ro / rw / z / cached / rprivate ...）。
func isMountMode(part string) bool {
	if part == "" {
		return false
	}
	for _, tok := range strings.Split(part, ",") {
		if _, ok := mountModes[tok]; !ok {
			return false
		}
	}
	return true
}

// cloneMap 通过 JSON 往返做一次深拷贝，避免与原对象共享底层 map。
func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	buf, err := json.Marshal(m)
	if err != nil {
		return m
	}
	out := map[string]any{}
	if err := json.Unmarshal(buf, &out); err != nil {
		return m
	}
	return out
}

func toStringSlice(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func toAnySlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func tagOf(ref string) string {
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		return ref[colon+1:]
	}
	return "latest"
}

// Package scanner 编排一次完整的更新巡检：
// 采集本机容器与镜像 → 与注册表比对 → 判定状态 → 生成并推送通知。
package scanner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/registry"
	"github.com/haihaipypy/autodocker/internal/store"
	"github.com/haihaipypy/autodocker/internal/version"
)

// ErrScanRunning 已有扫描在执行。
var ErrScanRunning = errors.New("已有扫描正在执行")

// Notifier 通知发送能力的抽象，避免 scanner 依赖具体通知实现。
type Notifier interface {
	// Notify 推送一条更新提醒，返回平台侧的消息 ID（用于后续更新卡片状态）。
	Notify(ctx context.Context, n *model.Notification) (string, error)
}

// Options 扫描配置。
type Options struct {
	ExtraWatch        []string // 额外的纯远端监控项
	ExcludeContainers []string // 不纳入监控的容器名
	SelfContainer     string
	SelfContainerID   string
	Concurrency       int

	// SkipImages 只扫容器维度，跳过「本机没被使用的镜像」。
	//
	// Agent（被监控端）必须打开：远端节点页只展示容器，镜像是纯白费。
	// 这段检测是串行的，且每个 tag 都要访问一次 registry —— 在访问
	// Docker Hub 需要走代理的机器上，几十个镜像能拖上几十分钟，
	// 而这期间 Hub 一条数据都收不到（上报在整轮扫完之后）。
	SkipImages bool
}

// Result 单个镜像的检测结果。
type Result struct {
	HasUpdate bool
	Notified  bool
	Skipped   bool
}

// Scanner 巡检器。
type Scanner struct {
	dk       *dockerx.Client
	reg      *registry.Client
	st       *store.Store
	notifier Notifier
	opt      Options
	log      *slog.Logger

	mu       sync.Mutex
	scanning bool
}

// New 构造巡检器。
func New(dk *dockerx.Client, reg *registry.Client, st *store.Store, opt Options, logger *slog.Logger) *Scanner {
	if opt.Concurrency <= 0 {
		opt.Concurrency = 4
	}
	return &Scanner{dk: dk, reg: reg, st: st, opt: opt, log: logger}
}

// SetNotifier 注入通知实现（可为空，为空时只落库不推送）。
func (s *Scanner) SetNotifier(n Notifier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifier = n
}

// SetOptions 热更新扫描配置。
//
// SkipImages 不跟着一起换：它标的是「这是个被监控端」，属于角色而不是配置，
// 设置页每次保存都会重建这份 Options，带不上的话会把 Agent 打回全量巡检。
func (s *Scanner) SetOptions(opt Options) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if opt.Concurrency <= 0 {
		opt.Concurrency = 4
	}
	opt.SkipImages = s.opt.SkipImages
	s.opt = opt
}

// Busy 是否有扫描在执行。
func (s *Scanner) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scanning
}

// Scan 执行一次巡检。force 为真时忽略去重，重新广播所有发现的差异。
func (s *Scanner) Scan(ctx context.Context, force bool) (*model.Scan, error) {
	s.mu.Lock()
	if s.scanning {
		s.mu.Unlock()
		return nil, ErrScanRunning
	}
	s.scanning = true
	opt := s.opt
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.scanning = false
		s.mu.Unlock()
	}()

	scanID, err := s.st.CreateScan(force)
	if err != nil {
		return nil, err
	}

	containers, err := s.dk.ListContainers(ctx)
	if err != nil {
		_ = s.st.FinishScan(scanID, 0, 0, 0, err.Error())
		return nil, err
	}

	live := make([]string, 0, len(containers))
	targets := make([]dockerx.Container, 0, len(containers))
	for _, c := range containers {
		if s.skip(c, opt) {
			continue
		}
		live = append(live, c.ID)
		targets = append(targets, c)
	}
	// 巡检可能跑几分钟（每个镜像都要问一次 registry），而 Agent 只有
	// 整轮结束才上报 —— 一行「开始」能让远端日志说清它到底卡在哪一步。
	s.log.Info("开始巡检", "containers", len(targets), "skip_images", opt.SkipImages)

	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		sem        = make(chan struct{}, opt.Concurrency)
		total      int
		withUpdate int
		notified   int
	)
	for _, c := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(c dockerx.Container) {
			defer wg.Done()
			defer func() { <-sem }()
			r := s.checkContainer(ctx, c, force)
			mu.Lock()
			total++
			if r.HasUpdate {
				withUpdate++
			}
			if r.Notified {
				notified++
			}
			mu.Unlock()
		}(c)
	}
	wg.Wait()

	// 镜像维度：本机存在但没有任何容器在用的镜像，加上 WATCH 的纯远端项。
	// 这一步必须放在容器之后 —— 判断「某个镜像有没有被使用」要靠容器列表。
	//
	// Agent 模式下整段跳过：它只负责把容器现状报给 Hub，节点页也只展示容器。
	// 而这段是串行的、每个 tag 都要访问 registry —— 在需要代理才能连上
	// Docker Hub 的机器上，几十个闲置镜像足以把首轮巡检拖到几十分钟。
	if !opt.SkipImages {
		iTotal, iUpdate, iNotified := s.checkImageTargets(ctx, opt, targets, force)
		total += iTotal
		withUpdate += iUpdate
		notified += iNotified
	}

	// 清理已消失容器的监控项
	if _, derr := s.st.DeleteImagesMissing(live); derr != nil {
		s.log.Warn("清理失效监控项失败", "err", derr)
	}

	if err := s.st.FinishScan(scanID, total, withUpdate, notified, ""); err != nil {
		s.log.Warn("写入扫描结果失败", "err", err)
	}
	s.log.Info("巡检完成", "total", total, "updates", withUpdate)
	sc, _ := s.st.GetScanByID(scanID)
	return sc, nil
}

// skip 判断容器是否应排除在监控之外。
func (s *Scanner) skip(c dockerx.Container, opt Options) bool {
	if c.Image == "" || strings.Contains(c.Image, "<none>") {
		return true
	}
	if c.Name == opt.SelfContainer {
		return true
	}
	if opt.SelfContainerID != "" && strings.HasPrefix(c.ID, opt.SelfContainerID) {
		return true
	}
	for _, ex := range opt.ExcludeContainers {
		if ex == "" {
			continue
		}
		if c.Name == ex || strings.HasPrefix(c.Name, ex) {
			return true
		}
	}
	// 自动删除的容器不适合重建（停止即被回收，无法回滚）
	if c.Labels["autodocker.skip"] == "true" {
		return true
	}
	return false
}

// checkContainer 检测单个本地容器。
func (s *Scanner) checkContainer(ctx context.Context, c dockerx.Container, force bool) Result {
	ref, err := registry.ParseReference(c.Image)
	if err != nil {
		s.log.Warn("无法解析镜像引用", "image", c.Image, "err", err)
		return Result{Skipped: true}
	}

	existing, findErr := s.st.FindImage(c.Image, c.ID)
	if findErr == nil && existing.Ignored {
		// 已忽略：只刷新本地观测值，不做远端查询
		s.upsertImage(existing, c, ref, model.StatusIgnored)
		return Result{Skipped: true}
	}

	mode := model.ModeAuto
	if findErr == nil && existing.Mode != "" {
		mode = existing.Mode
	}
	// effective 只在判断分支里用，**绝不写回数据库**。
	// 存「生效模式」会把自动判定固化住：nginx:latest 首轮被判成 digest-only 存下后，
	// 之后即使把容器换成 nginx:1.25，也再不会进入版本监听。
	effective := resolveMode(mode, ref.Tag)

	localDigest := ""
	if _, d, derr := s.dk.DigestForImage(ctx, c.ImageID, c.Image); derr == nil {
		localDigest = d
	} else {
		s.log.Warn("读取本地镜像摘要失败", "image", c.Image, "err", derr)
	}

	remoteDigest, rerr := s.reg.ManifestDigest(ctx, ref)
	if rerr != nil {
		img := s.upsertImage(existing, c, ref, model.StatusUnknown)
		img.Mode = mode
		img.LocalDigest = localDigest
		_ = s.st.UpdateImageCheck(img.ID, model.StatusUnknown, "", "", nil, mode, rerr.Error())
		return Result{Skipped: true}
	}

	status := model.StatusUpToDate
	latestTag := ""
	aliases := []string{}
	kind := model.NotifNewVersion

	if localDigest != "" && remoteDigest != "" && localDigest != remoteDigest {
		status = model.StatusUpdatePending
	} else if effective == model.ModePinWatch {
		// 锁定版本时，额外看仓库里有没有更高的版本 tag
		if t, a, ok := s.findNewerTags(ctx, ref); ok && t != "" {
			latestTag, aliases, status = t, a, model.StatusNewTag
		}
	}

	if status == model.StatusNewTag {
		kind = model.NotifNewTag
	}

	img := s.upsertImage(existing, c, ref, status)
	img.Mode = mode
	img.LocalDigest = localDigest
	img.RemoteDigest = remoteDigest
	img.LatestTag = latestTag
	img.Aliases = aliases
	if err := s.st.UpdateImageCheck(img.ID, status, remoteDigest, latestTag, aliases, mode, ""); err != nil {
		s.log.Warn("写入检测结果失败", "err", err)
	}

	if status != model.StatusUpdatePending && status != model.StatusNewTag {
		return Result{}
	}

	notified := s.notify(ctx, img, kind, force)
	return Result{HasUpdate: true, Notified: notified}
}

// imageTarget 一个镜像维度的监控目标：本机镜像，或仅在远端盯梢的引用。
type imageTarget struct {
	key      string             // 落库时写进 reference 字段的值
	ref      registry.Reference // 可解析出引用时才有；悬空镜像没有引用
	origin   model.ImageOrigin
	imageID  string
	size     int64
	created  int64
	dangling bool
}

// checkImageTargets 采集「镜像」维度的监控项。
//
// 与容器维度分开处理，是因为这两种东西的更新代价完全不同：容器要重建，
// 镜像只是重新拉取。判定口径与通知策略混在一起，迟早会出现
// 「一个谁也没在用的镜像发了新版，却给用户推了一张更新卡」的尴尬。
func (s *Scanner) checkImageTargets(ctx context.Context, opt Options, containers []dockerx.Container, force bool) (total, withUpdate, notified int) {
	images, err := s.dk.ListImages(ctx)
	if err != nil {
		// 拿不到镜像列表时绝不能往下清库：那会把所有镜像项当成「已消失」删光。
		s.log.Warn("列举本机镜像失败", "err", err)
	}

	inUse := make(map[string]bool, len(containers))
	for _, c := range containers {
		if c.ImageID != "" {
			inUse[c.ImageID] = true
		}
	}

	var targets []imageTarget
	localKeys := map[string]bool{}

	for _, img := range images {
		// 有容器在用的镜像不在这里重复建记录：容器维度那条已经覆盖它了，
		// 再建一条会让同一个镜像冒出两种状态、两个更新入口。
		if inUse[img.ID] {
			continue
		}
		if len(img.Tags) == 0 {
			// 悬空镜像没有引用可比对，只登记下来供清理，不参与版本检测。
			targets = append(targets, imageTarget{
				key: img.ID, origin: model.OriginLocal,
				imageID: img.ID, size: img.Size, created: img.Created, dangling: true,
			})
			continue
		}
		for _, tag := range img.Tags {
			ref, perr := registry.ParseReference(tag)
			if perr != nil {
				s.log.Debug("跳过无法解析的镜像引用", "tag", tag, "err", perr)
				continue
			}
			localKeys[ref.String()] = true
			targets = append(targets, imageTarget{
				key: ref.String(), ref: ref, origin: model.OriginLocal,
				imageID: img.ID, size: img.Size, created: img.Created,
			})
		}
	}

	// WATCH 里配的项若本机已有同名镜像，上面那步就接管了，这里跳过 ——
	// 否则同一个引用会被写两次，来源在 local 与 watch 之间来回横跳。
	for _, raw := range opt.ExtraWatch {
		ref, perr := registry.ParseReference(raw)
		if perr != nil {
			s.log.Warn("跳过非法的监控项", "ref", raw, "err", perr)
			continue
		}
		if localKeys[ref.String()] {
			continue
		}
		targets = append(targets, imageTarget{key: ref.String(), ref: ref, origin: model.OriginWatch})
	}

	// 串行处理即可：镜像项通常只有个位数，不值得为它撑起一套并发。
	liveLocal := make([]string, 0, len(targets))
	liveWatch := make([]string, 0, len(targets))
	for _, t := range targets {
		switch t.origin {
		case model.OriginLocal:
			liveLocal = append(liveLocal, t.key)
		case model.OriginWatch:
			liveWatch = append(liveWatch, t.key)
		}
		r := s.checkImageTarget(ctx, t, force)
		total++
		if r.HasUpdate {
			withUpdate++
		}
		if r.Notified {
			notified++
		}
	}

	if err == nil {
		if n, perr := s.st.PruneImages(model.OriginLocal, liveLocal); perr != nil {
			s.log.Warn("清理失效的镜像监控项失败", "err", perr)
		} else if n > 0 {
			s.log.Info("清理失效的镜像监控项", "count", n)
		}
	}
	if n, perr := s.st.PruneImages(model.OriginWatch, liveWatch); perr != nil {
		s.log.Warn("清理失效的远端监控项失败", "err", perr)
	} else if n > 0 {
		s.log.Info("清理失效的远端监控项", "count", n)
	}
	return total, withUpdate, notified
}

// checkImageTarget 处理一个镜像维度（或纯远端）的监控项。
//
// 镜像项刻意不推送通知：没人用的镜像出了新版，用户并不需要被打扰，
// 在页面上标一个「有更新」，他想清就清、想留就留，比推张卡片合适得多。
// 纯远端项（WATCH）照样通知 —— 那本来就是用户主动要求盯着的。
func (s *Scanner) checkImageTarget(ctx context.Context, t imageTarget, force bool) Result {
	existing, findErr := s.st.FindImage(t.key, "")
	if findErr == nil && existing.Ignored {
		return Result{Skipped: true}
	}

	mode := model.ModeAuto
	if findErr == nil && existing.Mode != "" {
		mode = existing.Mode
	}

	img := &model.Image{
		Reference:    t.key,
		Origin:       t.origin,
		ImageID:      t.imageID,
		SizeBytes:    t.size,
		ImageCreated: t.created,
		Dangling:     t.dangling,
		LocalTag:     t.ref.Tag,
		Status:       model.StatusUnknown,
		Mode:         mode,
	}

	// 悬空镜像没有 tag 可比，登记下来供清理就够了。
	if t.dangling {
		if _, err := s.st.UpsertImage(img); err != nil {
			s.log.Warn("写入悬空镜像失败", "id", t.imageID, "err", err)
		}
		return Result{Skipped: true}
	}

	effective := resolveMode(mode, t.ref.Tag)

	localDigest := ""
	if t.imageID != "" {
		if _, d, derr := s.dk.DigestForImage(ctx, t.imageID, t.key); derr == nil {
			localDigest = d
		} else {
			s.log.Debug("读取本地镜像摘要失败", "ref", t.key, "err", derr)
		}
	}
	img.LocalDigest = localDigest

	remoteDigest, rerr := s.reg.ManifestDigest(ctx, t.ref)
	if rerr != nil {
		id, uerr := s.st.UpsertImage(img)
		if uerr != nil {
			s.log.Warn("写入镜像监控项失败", "ref", t.key, "err", uerr)
			return Result{Skipped: true}
		}
		_ = s.st.UpdateImageCheck(id, model.StatusUnknown, "", "", nil, mode, rerr.Error())
		return Result{Skipped: true}
	}
	img.RemoteDigest = remoteDigest

	status := model.StatusUpToDate
	switch t.origin {
	case model.OriginWatch:
		// 纯远端项没有本地副本，只能拿库里上次记下的远端摘要当基线。
		base := ""
		if existing != nil {
			base = existing.RemoteDigest
		}
		if base != "" && base != remoteDigest {
			status = model.StatusUpdatePending
		}
	default:
		// 本机镜像：本地这份与仓库当前内容对不上，就说明本地这份落后了。
		if localDigest != "" && localDigest != remoteDigest {
			status = model.StatusUpdatePending
		}
	}

	latestTag := ""
	aliases := []string{}
	if status == model.StatusUpToDate && effective == model.ModePinWatch {
		if lt, al, ok := s.findNewerTags(ctx, t.ref); ok && lt != "" {
			latestTag, aliases, status = lt, al, model.StatusNewTag
		}
	}

	id, uerr := s.st.UpsertImage(img)
	if uerr != nil {
		s.log.Warn("写入镜像监控项失败", "ref", t.key, "err", uerr)
		return Result{Skipped: true}
	}
	if err := s.st.UpdateImageCheck(id, status, remoteDigest, latestTag, aliases, mode, ""); err != nil {
		s.log.Warn("写入检测结果失败", "err", err)
	}

	if status != model.StatusUpdatePending && status != model.StatusNewTag {
		return Result{}
	}
	if t.origin != model.OriginWatch {
		// 镜像项只在页面上标状态，不打扰人。
		return Result{HasUpdate: true}
	}

	img.ID = id
	img.Status = status
	img.LatestTag = latestTag
	img.Aliases = aliases
	// checkImageTarget 这里的 img 是现拼的，不像容器路径那样继承了 existing，
	// 得手动补上已放行的摘要，notify 才能判断「这一版用户是不是已经放行了」。
	if existing != nil {
		img.IgnoredDigest = existing.IgnoredDigest
		img.NotifiedTag = existing.NotifiedTag
		img.NotifiedDigest = existing.NotifiedDigest
	}
	kind := model.NotifNewVersion
	if status == model.StatusNewTag {
		kind = model.NotifNewTag
	}
	notified := s.notify(ctx, img, kind, force)
	return Result{HasUpdate: true, Notified: notified}
}

// findNewerTags 查询仓库中是否存在比当前 tag 更高的版本。
//
// 返回的 ok 为 false 表示查询本身失败（网络、限流等），
// 与「查成功但没有更高版本」区分开，便于调用方决定是否降级处理。
func (s *Scanner) findNewerTags(ctx context.Context, ref registry.Reference) (latest string, aliases []string, ok bool) {
	tags, err := s.reg.Tags(ctx, ref)
	if err != nil {
		s.log.Warn("拉取 tag 列表失败", "image", ref.String(), "err", err)
		return "", nil, false
	}
	newer := version.NewerThan(ref.Tag, tags)
	if len(newer) == 0 {
		return "", nil, true
	}
	aliases = newer[1:]
	if len(aliases) > 4 {
		aliases = aliases[:4]
	}
	return newer[0], aliases, true
}

// upsertImage 把容器观测值落库（保留用户侧设定与基线）。
func (s *Scanner) upsertImage(existing *model.Image, c dockerx.Container, ref registry.Reference, status model.Status) *model.Image {
	img := &model.Image{
		Reference:     c.Image,
		ContainerID:   c.ID,
		ContainerName: c.Name,
		Origin:        model.OriginContainer,
		ImageID:       c.ImageID,
		LocalTag:      ref.Tag,
		Status:        status,
		Mode:          model.ModeAuto,
	}
	if existing != nil {
		img.ID = existing.ID
		img.BaselineAt = existing.BaselineAt
		img.CreatedAt = existing.CreatedAt
		img.Mode = existing.Mode
		img.ModeOverride = existing.ModeOverride
		img.Ignored = existing.Ignored
		// 带上「已放行的远端摘要」，否则 notify 里的抑制判断拿不到值，
		// 用户点过的「忽略本版本」会在下一轮扫描后失效。
		img.IgnoredDigest = existing.IgnoredDigest
		// 同理带上「提醒过哪一版」。这两个值一旦丢了，用户清掉通知历史后
		// 同一版本就会重新冒出来 —— 而且看起来像在抽风，因为卡片明明点过了。
		img.NotifiedTag = existing.NotifiedTag
		img.NotifiedDigest = existing.NotifiedDigest
		img.Aliases = existing.Aliases
		img.LatestTag = existing.LatestTag
	}
	if _, err := s.st.UpsertImage(img); err != nil {
		s.log.Warn("写入监控项失败", "ref", c.Image, "err", err)
	}
	return img
}

// notify 去重后生成并推送通知，返回是否真的推送了。
func (s *Scanner) notify(ctx context.Context, img *model.Image, kind model.NotificationKind, force bool) bool {
	if !force {
		// 同一条监控项已有待处理的通知时不再重复打扰
		if _, err := s.st.FindPendingNotification(img.Reference, img.ContainerID); err == nil {
			return false
		}
		// 这一版已经提醒过，或者用户点过「忽略本版本」。
		//
		// 判断依据记在**监控项**上，不是通知记录里：用户清掉已读历史之后那条
		// 记录就没了，还照它判断等于「删一条通知就让同一版本重新冒出来」——
		// 这正是「忽略或移除的提醒总是又出现」的根因。
		//
		// HasNotificationForTag 留着兜底，覆盖升级前的老数据：那时还没有
		// notified_tag，历史记录是唯一的依据。
		if kind == model.NotifNewTag && img.LatestTag != "" {
			if img.NotifiedTag != "" && version.SameVersion(img.NotifiedTag, img.LatestTag) {
				return false
			}
			if ok, err := s.st.HasNotificationForTag(img.Reference, img.ContainerID, img.LatestTag); err == nil && ok {
				return false
			}
		}
		// 用户在卡片上点过「忽略本版本」：只要远端内容没再变，就别反复提醒。
		//
		// 这里必须比**远端摘要**而不是 tag：浮动 tag 场景下 tag 永远不变，
		// 能区分「还是那一版」和「作者又推了新一版」的只有摘要。
		// new_tag 不需要这一步 —— 上面按 tag 比过了。
		if kind == model.NotifNewVersion && img.RemoteDigest != "" &&
			(img.NotifiedDigest == img.RemoteDigest || img.IgnoredDigest == img.RemoteDigest) {
			return false
		}
	}

	n := &model.Notification{
		ImageRef:      img.Reference,
		ContainerID:   img.ContainerID,
		ContainerName: img.ContainerName,
		Kind:          kind,
		LocalSummary:  version.ShortDigest(img.LocalDigest),
		RemoteSummary: version.ShortDigest(img.RemoteDigest),
		LocalTag:      img.LocalTag,
		LatestTag:     img.LatestTag,
		Aliases:       img.Aliases,
		CardState:     model.CardPending,
	}
	n.Title, n.Body = buildText(img, kind)

	id, err := s.st.CreateNotification(n)
	if err != nil {
		s.log.Warn("写入通知失败", "err", err)
		return false
	}
	n.ID = id

	// 记下「这一版说过了」，位置必须在推送之前 —— 没配渠道时下面会直接返回，
	// 而页面上那条提醒已经存在，下一轮不该再补一条。
	//
	// 两个标记分开写：new_tag 场景的 RemoteDigest 是**旧 tag** 的摘要，
	// 拿它当「已提醒的摘要」会在下一轮把新版本当成「提醒过了」而漏掉。
	if kind == model.NotifNewTag {
		_ = s.st.SetImageNotified(img.ID, img.LatestTag, "")
	} else if img.RemoteDigest != "" {
		_ = s.st.SetImageNotified(img.ID, "", img.RemoteDigest)
	}

	s.mu.Lock()
	notifier := s.notifier
	s.mu.Unlock()
	if notifier == nil {
		return true
	}
	// 推送放在独立超时里，避免拖慢整体扫描
	pushCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	msgID, nerr := notifier.Notify(pushCtx, n)
	if nerr != nil {
		s.log.Warn("推送通知失败", "ref", img.Reference, "err", nerr)
		return true
	}
	if msgID != "" || len(n.Deliveries) > 0 {
		// 投递坐标由通知实现填回 n 上：同一条提醒可能同时发到飞书和 Telegram，
		// 两边的消息 ID 互不相干，事后要改状态得知道各去哪个平台的哪条会话找哪条消息。
		_ = s.st.SetNotificationDeliveries(id, n.Deliveries, model.CardPending)
	}
	return true
}

// buildText 生成通知标题与正文。
func buildText(img *model.Image, kind model.NotificationKind) (title, body string) {
	name := img.ContainerName
	if name == "" {
		name = img.Reference
	}
	switch kind {
	case model.NotifNewTag:
		title = fmt.Sprintf("%s 有新版本可用", name)
		body = fmt.Sprintf("当前版本 `%s`，仓库中已存在更高版本 **%s**", img.LocalTag, img.LatestTag)
		if len(img.Aliases) > 0 {
			body += fmt.Sprintf("（同版本别名：%s）", strings.Join(img.Aliases, "、"))
		}
		body += "。\n点击下方按钮更新容器，更新过程会自动保留原有挂载、端口与网络配置。"
	default:
		title = fmt.Sprintf("%s 的镜像有更新", name)
		body = fmt.Sprintf("镜像 `%s` 的远端内容已变更（%s → %s）。\n点击下方按钮更新容器，更新过程会自动保留原有挂载、端口与网络配置。",
			img.Reference, orDash(version.ShortDigest(img.LocalDigest)), orDash(version.ShortDigest(img.RemoteDigest)))
	}
	return title, body
}

func orDash(s string) string {
	if s == "" {
		return "未知"
	}
	return s
}

// resolveMode 决定实际使用的检测模式。
//
// 自动模式下的规则与 vigil 一致：
// 数字版本号 tag（1.25 / v3.9 / 1.2.3-alpine）走 pin-watch，
// 浮动 tag（latest / stable / edge）走 digest-only。
func resolveMode(mode model.DetectMode, tag string) model.DetectMode {
	if mode != "" && mode != model.ModeAuto {
		return mode
	}
	if version.IsVersionTag(tag) {
		return model.ModePinWatch
	}
	return model.ModeDigestOnly
}

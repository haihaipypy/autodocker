// Package scanner 编排一次完整的更新巡检：
// 采集本机容器与镜像 → 与注册表比对 → 判定状态 → 生成并推送通知。
package scanner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/registry"
	"github.com/haihaipypy/autodocker/internal/selfupgrade"
	"github.com/haihaipypy/autodocker/internal/store"
	"github.com/haihaipypy/autodocker/internal/version"
)

// ErrScanRunning 已有扫描在执行。
var ErrScanRunning = errors.New("已有扫描正在执行")

// composeProjectLabel 是 compose 打在容器上的项目名标签。
//
// 合并提醒的唯一可靠依据：靠容器名前缀猜（safeline-chaos → safeline）在
// 自定义了 container_name 或项目名时立刻失效，而标签是 compose 自己写的。
const composeProjectLabel = "com.docker.compose.project"

// composeServiceLabel / composeDependsLabel 是 compose 打在容器上的
// 「服务名」与「依赖谁」两个标签。
//
// depends_on 的值长这样：
//
//	safeline-pg:service_started:false,safeline-mgt:service_healthy:true
//
// 每一项是 服务名:启动条件:是否必须。**里面写的是服务名，不是容器名** ——
// 容器名还带着项目前缀和 -1 序号，直接拿容器名去比对会一个都对不上，
// 所以这两个标签必须一起取。
const (
	composeServiceLabel = "com.docker.compose.service"
	composeDependsLabel = "com.docker.compose.depends_on"
)

// parseComposeDeps 把 depends_on 标签拆成服务名列表。
//
// 只保留服务名：启动条件（service_started / service_healthy）与「是否必须」
// 我们不用 —— 重建是自己停自己起，条件由重建后的等待健康检查来保证。
// 顺序与去重就地做掉，脏值（空项、只有一个冒号的残缺项）直接丢弃。
func parseComposeDeps(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	seen := make(map[string]bool, 4)
	for _, item := range strings.Split(raw, ",") {
		name := item
		if i := strings.IndexByte(item, ':'); i >= 0 {
			name = item[:i]
		}
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

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
	// 排除名单里「自身」相关的名字必须按实际容器名来拼，不能只信配置。
	// 配置默认是 autodocker，而 Agent 装出来叫 autodocker-agent —— 拼出来的
	// autodocker-old 永远对不上列表里的 autodocker-agent-old，那个「改名待删」
	// 的旧容器就被当成用户的容器上报，还带着「可选更新」。
	opt = withSelfExcludes(opt, containers)
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
		hits       []updateHit
	)
	for _, c := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(c dockerx.Container) {
			defer wg.Done()
			defer func() { <-sem }()
			r, hit := s.checkContainer(ctx, c, force)
			mu.Lock()
			total++
			if r.HasUpdate {
				withUpdate++
			}
			if hit != nil {
				hits = append(hits, *hit)
			}
			mu.Unlock()
		}(c)
	}
	wg.Wait()

	// 通知必须在整轮容器扫完之后再发：同一 compose 项目里的服务是一起发版的，
	// 边扫边发就是一次弹 6 条消息。收集起来按项目合并，一个项目只发一条。
	notified := s.notifyHits(ctx, hits, force)

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

// withSelfExcludes 按容器列表里**实际**的自身名字，补上自升级过程出现的两个名字。
//
// 为什么不能只靠 Options 里的配置：SelfContainer 的默认值是 autodocker（历史原因，
// 那时只有 Hub 一种形态），而 Agent 的安装命令把容器名写死成 autodocker-agent。
// 少了这一步，排除名单里躺着的是 autodocker-old，而页面上冒出来的是
// autodocker-agent-old —— 两者对不上，那个容器就一直挂在节点列表里。
//
// 认自身优先按 ID（容器里的 hostname 就是短 ID，最可靠），其次按名字。
func withSelfExcludes(opt Options, list []dockerx.Container) Options {
	for _, c := range list {
		if c.Name != opt.SelfContainer &&
			(opt.SelfContainerID == "" || !strings.HasPrefix(c.ID, opt.SelfContainerID)) {
			continue
		}
		extra := selfupgrade.AuxContainers(c.Name)
		opt.ExcludeContainers = append(append([]string{}, opt.ExcludeContainers...), extra...)
		return opt
	}
	return opt
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

// resolveImageRef 把「用镜像 ID 起的容器」还原成它真正的仓库引用。
//
// Config.Image 里存的本来应该是仓库引用，但用 docker run <镜像ID> 起的容器、
// 或在面板里按 ID 重建过的容器，存的就是一串 ID。把这串 ID 当引用去查注册表，
// 得到的是「library/c4d252255b7b:latest 不存在」—— 用户看到「所有镜像源均失败」，
// 跑去换加速源、查网络，而真正的问题跟镜像源毫无关系。
//
// 镜像身上通常还挂着当初拉取时的仓库名，就地取回来即可。取不回来时保持原样，
// 由调用方如实说明「查不了」，不硬编一个假引用。
func (s *Scanner) resolveImageRef(ctx context.Context, c dockerx.Container) dockerx.Container {
	if !registry.IsImageID(c.Image) {
		return c
	}
	id := c.ImageID
	if id == "" {
		id = c.Image
	}
	tags, _, err := s.dk.ImageNames(ctx, id)
	if err != nil {
		s.log.Debug("读取镜像仓库名失败", "container", c.Name, "image", c.Image, "err", err)
		return c
	}
	ref, ok := pickImageRef(tags)
	if !ok {
		return c
	}
	// 老记录是按那串 ID 存的，直接换引用会多出一条：唯一键是「引用 + 容器号」，
	// 引用变了就是新键。就地改掉老记录，用户的「忽略」「检测模式」这些设定才不会丢。
	if old, err := s.st.FindImage(c.Image, c.ID); err == nil {
		if uerr := s.st.UpdateImageAfterRebuild(old.ID, ref, c.ImageID, c.ID, c.Name, old.LocalDigest, ""); uerr != nil {
			s.log.Warn("迁移镜像记录到仓库引用失败", "image", c.Image, "ref", ref, "err", uerr)
		}
	}
	s.log.Info("按镜像 ID 找回仓库引用", "container", c.Name, "image", c.Image, "ref", ref)
	c.Image = ref
	return c
}

// pickImageRef 从镜像登记的仓库名里挑一个用来查更新的引用。
//
// 同一个镜像可能同时挂着好几个名字（:latest 与 :9.4.7 并存，或迁移时留下过
// 内部仓库地址）。挑法固定：Docker Hub 的排在其它注册表前面，带版本号的排在
// latest 前面，剩下的按名字排 —— 每轮挑中的必须是同一个，否则今天盯 :9.4.7、
// 明天盯 :latest，本地版本这一栏会来回跳。
//
// 版本号优先是因为它还能带来「上游出了更新版本」的提醒；latest 只能比对
// 内容有没有被偷偷改掉，看不出新版本。
func pickImageRef(tags []string) (string, bool) {
	type cand struct {
		ref  string
		rank int
	}
	cands := make([]cand, 0, len(tags))
	for _, t := range tags {
		if strings.Contains(t, "<none>") {
			continue
		}
		r, err := registry.ParseReference(t)
		if err != nil {
			continue
		}
		rank := 0
		if r.Registry != registry.DefaultRegistryAPI {
			rank += 2 // 先不信任别的注册表：多半是迁移时留下的旧地址
		}
		if r.Tag == "latest" {
			rank++
		}
		cands = append(cands, cand{ref: t, rank: rank})
	}
	if len(cands) == 0 {
		return "", false
	}
	slices.SortFunc(cands, func(a, b cand) int {
		if a.rank != b.rank {
			return a.rank - b.rank
		}
		return strings.Compare(a.ref, b.ref)
	})
	return cands[0].ref, true
}

// markUncheckable 记下「这个容器查不了更新」，并写明为什么。
//
// 状态仍是 unknown（确实没查到结果），但原因要说清是「没有引用可查」而不是
// 「镜像源不给你查」—— 后者会让用户去换加速源，换多少次都没用。
func (s *Scanner) markUncheckable(existing *model.Image, c dockerx.Container) Result {
	status := model.StatusUnknown
	if existing != nil && existing.Ignored {
		status = model.StatusIgnored
	}
	img := s.upsertImage(existing, c, registry.Reference{}, status, "")
	if status == model.StatusIgnored {
		return Result{Skipped: true}
	}
	_ = s.st.UpdateImageCheck(img.ID, status, "", "", nil, img.Mode, uncheckableReason(c))
	return Result{Skipped: true}
}

// uncheckableReason 说明为什么这个容器查不了更新，并给出能照着做的一步。
//
// 「把它改成带 tag 的完整引用」这种话在这里没用 —— 页面上没有改引用的入口，
// 容器的 Config.Image 也是创建时就定死的，用户看完只能干瞪眼。compose 起的
// 容器带着项目、服务名和工作目录，直接给重建命令，那才是他能执行的动作。
func uncheckableReason(c dockerx.Container) string {
	service := c.Labels["com.docker.compose.service"]
	if service != "" {
		where := "compose 目录"
		if dir := c.Labels["com.docker.compose.project.working_dir"]; dir != "" {
			where = dir
		}
		return fmt.Sprintf("镜像已无标签（只剩 ID），本机查不到仓库名。"+
			"到 %s 执行 docker compose up -d --force-recreate %s 重建一次即可恢复检测。", where, service)
	}
	return fmt.Sprintf("镜像只有 ID（%s），本机找不到它对应的仓库名。"+
		"用原来的镜像名重跑一次这个容器即可恢复检测。", c.Image)
}

// checkContainer 检测单个本地容器。
func (s *Scanner) checkContainer(ctx context.Context, c dockerx.Container, force bool) (Result, *updateHit) {
	c = s.resolveImageRef(ctx, c)

	ref, err := registry.ParseReference(c.Image)
	if err != nil {
		s.log.Warn("无法解析镜像引用", "image", c.Image, "err", err)
		return Result{Skipped: true}, nil
	}

	existing, findErr := s.st.FindImage(c.Image, c.ID)

	// 找不回仓库名的镜像 ID：不去问注册表。
	// 「library/c4d252255b7b:latest」这种引用语法合法、语义是假的，问出去只会
	// 换来一串「镜像或 tag 不存在」，把用户永久钉在「检测异常」上。
	if registry.IsImageID(c.Image) {
		return s.markUncheckable(existing, c), nil
	}

	// 本地摘要必须和 upsert 一起落库。
	//
	// 只在后面的判定里用、却忘了存的表现很隐蔽：本机页面看不出问题（通知是拿
	// 内存里的值拼的），但远端节点把库里的记录整份上报给 Hub，那一栏就是空的 ——
	// 卡片上写着「本地摘要：未知」，用户没法确认到底是哪一份镜像落后了。
	localDigest := ""
	if _, d, derr := s.dk.DigestForImage(ctx, c.ImageID, c.Image); derr == nil {
		localDigest = d
	} else {
		s.log.Warn("读取本地镜像摘要失败", "image", c.Image, "err", derr)
	}

	if findErr == nil && existing.Ignored {
		// 已忽略：只刷新本地观测值，不做远端查询
		s.upsertImage(existing, c, ref, model.StatusIgnored, localDigest)
		return Result{Skipped: true}, nil
	}

	mode := model.ModeAuto
	if findErr == nil && existing.Mode != "" {
		mode = existing.Mode
	}
	// effective 只在判断分支里用，**绝不写回数据库**。
	// 存「生效模式」会把自动判定固化住：nginx:latest 首轮被判成 digest-only 存下后，
	// 之后即使把容器换成 nginx:1.25，也再不会进入版本监听。
	effective := resolveMode(mode, ref.Tag)

	remoteDigest, rerr := s.reg.ManifestDigest(ctx, ref)
	if rerr != nil {
		img := s.upsertImage(existing, c, ref, model.StatusUnknown, localDigest)
		img.Mode = mode
		_ = s.st.UpdateImageCheck(img.ID, model.StatusUnknown, "", "", nil, mode, rerr.Error())
		return Result{Skipped: true}, nil
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

	img := s.upsertImage(existing, c, ref, status, localDigest)
	img.Mode = mode
	img.RemoteDigest = remoteDigest
	img.LatestTag = latestTag
	img.Aliases = aliases
	if err := s.st.UpdateImageCheck(img.ID, status, remoteDigest, latestTag, aliases, mode, ""); err != nil {
		s.log.Warn("写入检测结果失败", "err", err)
	}

	if status != model.StatusUpdatePending && status != model.StatusNewTag {
		return Result{}, nil
	}

	// 不在这里发通知：先攒着，整轮扫完后按 compose 项目合并成一条。
	return Result{HasUpdate: true}, &updateHit{img: img, kind: kind}
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
	notified := s.notifyOne(ctx, img, kind, force)
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
//
// localDigest 必须在这里写进去：UpsertImage 的冲突分支会用新值覆盖 local_digest，
// 传空就等于每轮巡检都把这一列抹一遍。
func (s *Scanner) upsertImage(existing *model.Image, c dockerx.Container, ref registry.Reference, status model.Status, localDigest string) *model.Image {
	img := &model.Image{
		Reference:      c.Image,
		ContainerID:    c.ID,
		ContainerName:  c.Name,
		ComposeProject: c.Labels[composeProjectLabel],
		ComposeService: c.Labels[composeServiceLabel],
		ComposeDeps:    parseComposeDeps(c.Labels[composeDependsLabel]),
		Origin:         model.OriginContainer,
		ImageID:        c.ImageID,
		LocalTag:       ref.Tag,
		LocalDigest:    localDigest,
		Status:         status,
		Mode:           model.ModeAuto,
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

// updateHit 这一轮巡检里「该提醒」的一个容器。
//
// 不在 checkContainer 里直接发通知：同一 compose 项目的服务是一起发版的，
// 边扫边发就是一次弹 6 条消息。先攒着，整轮扫完按项目合并。
type updateHit struct {
	img  *model.Image
	kind model.NotificationKind
}

// shouldNotify 这一版是不是该提醒（去重）。
func (s *Scanner) shouldNotify(img *model.Image, kind model.NotificationKind, force bool) bool {
	if force {
		return true
	}
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
	return true
}

// notifyHits 去重、按 compose 项目合并后落库并推送，返回提醒覆盖的容器数。
func (s *Scanner) notifyHits(ctx context.Context, hits []updateHit, force bool) int {
	ready := make([]updateHit, 0, len(hits))
	for _, h := range hits {
		if s.shouldNotify(h.img, h.kind, force) {
			ready = append(ready, h)
		}
	}
	count := 0
	for _, group := range groupHits(ready) {
		if s.notifyGroup(ctx, group) {
			count += len(group)
		}
	}
	return count
}

// notifyOne 单条提醒（镜像维度那种没有 compose 项目的项）。
func (s *Scanner) notifyOne(ctx context.Context, img *model.Image, kind model.NotificationKind, force bool) bool {
	return s.notifyHits(ctx, []updateHit{{img: img, kind: kind}}, force) > 0
}

// groupHits 按 compose 项目分组，没有项目标签的容器各自一组。
//
// 顺序按第一次出现的容器来，不排序：用户看提醒时对照的就是扫描顺序，
// 突然按字母重排反而对不上自己在页面上看到的那一列。
func groupHits(hits []updateHit) [][]updateHit {
	idx := map[string]int{}
	var out [][]updateHit
	for _, h := range hits {
		key := h.img.ComposeProject
		if key == "" {
			// 没有项目标签：容器编号唯一；纯远端盯梢项没有容器，用记录 ID 兜底。
			key = "#" + h.img.ContainerID
			if h.img.ContainerID == "" {
				key = "#img:" + strconv.FormatInt(h.img.ID, 10)
			}
		}
		if i, ok := idx[key]; ok {
			out[i] = append(out[i], h)
			continue
		}
		idx[key] = len(out)
		out = append(out, []updateHit{h})
	}
	return out
}

// notifyGroup 把一组（同一 compose 项目）更新项写成一条提醒并推送。
func (s *Scanner) notifyGroup(ctx context.Context, group []updateHit) bool {
	if len(group) == 0 {
		return false
	}
	first := group[0].img
	n := &model.Notification{
		ImageRef:       first.Reference,
		ContainerID:    first.ContainerID,
		ContainerName:  first.ContainerName,
		Kind:           group[0].kind,
		ComposeProject: first.ComposeProject,
		LocalSummary:   version.ShortDigest(first.LocalDigest),
		RemoteSummary:  version.ShortDigest(first.RemoteDigest),
		LocalTag:       first.LocalTag,
		LatestTag:      first.LatestTag,
		Aliases:        first.Aliases,
		CardState:      model.CardPending,
	}
	members := make([]model.NotificationMember, 0, len(group))
	for _, h := range group {
		members = append(members, model.NotificationMember{
			ContainerID:   h.img.ContainerID,
			ContainerName: h.img.ContainerName,
			ImageRef:      h.img.Reference,
			Kind:          h.kind,
			LocalTag:      h.img.LocalTag,
			LatestTag:     h.img.LatestTag,
			LocalSummary:  version.ShortDigest(h.img.LocalDigest),
			RemoteSummary: version.ShortDigest(h.img.RemoteDigest),
		})
	}
	n.Title, n.Body = buildGroupText(group)

	id, err := s.st.CreateNotificationWithMembers(n, members)
	if err != nil {
		s.log.Warn("写入通知失败", "project", first.ComposeProject, "ref", first.Reference, "err", err)
		return false
	}
	n.ID = id

	// 记下「这一版说过了」，位置必须在推送之前 —— 没配渠道时下面会直接返回，
	// 而页面上那条提醒已经存在，下一轮不该再补一条。
	//
	// 两个标记分开写：new_tag 场景的 RemoteDigest 是**旧 tag** 的摘要，
	// 拿它当「已提醒的摘要」会在下一轮把新版本当成「提醒过了」而漏掉。
	for _, h := range group {
		if h.kind == model.NotifNewTag {
			_ = s.st.SetImageNotified(h.img.ID, h.img.LatestTag, "")
		} else if h.img.RemoteDigest != "" {
			_ = s.st.SetImageNotified(h.img.ID, "", h.img.RemoteDigest)
		}
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
		s.log.Warn("推送通知失败", "ref", first.Reference, "err", nerr)
		return true
	}
	if msgID != "" || len(n.Deliveries) > 0 {
		// 投递坐标由通知实现填回 n 上：同一条提醒可能同时发到飞书和 Telegram，
		// 两边的消息 ID 互不相干，事后要改状态得知道各去哪个平台的哪条会话找哪条消息。
		_ = s.st.SetNotificationDeliveries(id, n.Deliveries, model.CardPending)
	}
	return true
}

// buildGroupText 合并提醒的标题与正文。
//
// 标题必须一眼能看出「这是整个项目，不是某一个容器」：手机锁屏上只看得到
// 第一行，写「safeline-chaos 有新版本」会让用户以为只有那一个要更新。
func buildGroupText(group []updateHit) (title, body string) {
	first := group[0].img
	if len(group) == 1 {
		return buildText(first, group[0].kind)
	}
	project := first.ComposeProject
	if project == "" {
		project = "同一批容器"
	}
	title = fmt.Sprintf("%s（%d 个容器）有新版本可用", project, len(group))

	var b strings.Builder
	fmt.Fprintf(&b, "compose 项目 **%s** 有 %d 个容器可更新：", project, len(group))
	limit := len(group)
	if limit > 12 {
		limit = 12
	}
	for _, h := range group[:limit] {
		name := h.img.ContainerName
		if name == "" {
			name = h.img.Reference
		}
		if h.kind == model.NotifNewTag && h.img.LatestTag != "" {
			fmt.Fprintf(&b, "\n• %s  %s → **%s**", name, h.img.LocalTag, h.img.LatestTag)
		} else {
			fmt.Fprintf(&b, "\n• %s  %s（内容已更新）", name, h.img.LocalTag)
		}
	}
	if len(group) > limit {
		fmt.Fprintf(&b, "\n…另有 %d 个", len(group)-limit)
	}
	b.WriteString("\n\n点「立即更新」会依次重建这些容器，一次一个；某个失败会停下并说明原因。")
	return title, b.String()
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

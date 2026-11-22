package scanner

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/haihaipypy/autodocker/internal/dockerx"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/registry"
	"github.com/haihaipypy/autodocker/internal/selfupgrade"
	"github.com/haihaipypy/autodocker/internal/store"
)

// 通知正文是用户唯一能看到的解释，措辞不通顺只能靠用户实机反馈才发现
// （本轮就是「当前锁定 2」被指出读不懂）。钉住关键措辞，防止以后改写时漂回去。
func TestBuildTextNewTagWording(t *testing.T) {
	img := &model.Image{
		ContainerName: "ad-registry",
		Reference:     "registry:2",
		LocalTag:      "2",
		LatestTag:     "3.1.2",
		Aliases:       []string{"3.1.1", "3.1"},
	}
	title, body := buildText(img, model.NotifNewTag)

	if title != "ad-registry 有新版本可用" {
		t.Errorf("标题不对: %q", title)
	}
	if !strings.Contains(body, "当前版本 `2`") {
		t.Errorf("正文应写成「当前版本」，实际: %q", body)
	}
	// 「当前锁定」是内部机制的说法（pin-watch 锁 tag），不该出现在给用户看的文案里
	if strings.Contains(body, "当前锁定") {
		t.Errorf("正文不该再出现「当前锁定」: %q", body)
	}
	if !strings.Contains(body, "**3.1.2**") {
		t.Errorf("正文缺少更高版本号: %q", body)
	}
	if !strings.Contains(body, "同版本别名：3.1.1、3.1") {
		t.Errorf("正文缺少同版本别名: %q", body)
	}
}

// 容器名缺失时要退回镜像引用，否则标题会以空格开头，看着像渲染出错。
func TestBuildTextTitleFallsBackToReference(t *testing.T) {
	img := &model.Image{
		Reference:    "nginx:1.25.0",
		LocalDigest:  "sha256:aaaaaaaaaaaa",
		RemoteDigest: "sha256:bbbbbbbbbbbb",
	}
	title, _ := buildText(img, model.NotifNewVersion)
	if !strings.Contains(title, "nginx:1.25.0") {
		t.Errorf("无容器名时标题应含镜像引用，实际: %q", title)
	}
}

// 用户点过「忽略本版本」后，远端摘要没变就不能再推，变了才推。
//
// 这是「忽略」按钮真正的语义所在：拦的是**某一个远端内容**，不是永远闭嘴。
// 老实现只改通知卡片状态，这里就把扫描端这条短路钉死，防止以后又被改回去。
func TestNotifySuppressesOnlyIgnoredDigest(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	sc := &Scanner{st: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	const ref, cid = "127.0.0.1:5000/myapp:latest", "c1"

	id, err := st.UpsertImage(&model.Image{
		Reference: ref, ContainerID: cid, ContainerName: "app",
		Origin: model.OriginContainer, Status: model.StatusUpdatePending,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 模拟扫描已经拿到远端摘要、用户点了「忽略本版本」
	if err := st.SetImageIgnoredDigest(id, "sha256:new"); err != nil {
		t.Fatal(err)
	}
	img, err := st.FindImage(ref, cid)
	if err != nil {
		t.Fatal(err)
	}
	img.RemoteDigest = "sha256:new"

	// 远端摘要没变 → 抑制
	if sc.notifyOne(ctx, img, model.NotifNewVersion, false) {
		t.Error("已忽略的这一版不应再次推送通知")
	}

	// 仓库又推了新内容（摘要变了）→ 照常推送
	img.RemoteDigest = "sha256:newer"
	if !sc.notifyOne(ctx, img, model.NotifNewVersion, false) {
		t.Error("远端内容已变化时应重新推送通知")
	}
}

// 清空通知历史之后，同一版本不能再冒出来。
//
// 这是 issue 里「移除的提醒总是又出现」的直接复现：去重原先只认通知记录，
// 用户点一次「清除已读」，记录就没了，下一轮扫描认为「从没提醒过」，
// 于是把同一个 v3.7.0 再推一遍 —— 点几次都清不掉。
func TestNotifySurvivesNotificationCleanup(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	sc := &Scanner{st: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	const ref, cid = "gdy666/lucky:v3.6.0", "c1"

	if _, err := st.UpsertImage(&model.Image{
		Reference: ref, ContainerID: cid, ContainerName: "lucky",
		Origin: model.OriginContainer, LocalTag: "v3.6.0",
		LatestTag: "v3.7.0", Status: model.StatusNewTag,
	}); err != nil {
		t.Fatal(err)
	}

	img, err := st.FindImage(ref, cid)
	if err != nil {
		t.Fatal(err)
	}

	if !sc.notifyOne(ctx, img, model.NotifNewTag, false) {
		t.Fatal("第一轮应当推送提醒")
	}

	// 用户看过之后清空历史 —— 这一步正是「提醒又出现」的触发动作。
	if err := st.MarkAllRead(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteReadNotifications(); err != nil {
		t.Fatal(err)
	}

	// 重新读一遍，模拟下一轮扫描（标记是落库的，内存里那份不会自己变）。
	img, err = st.FindImage(ref, cid)
	if err != nil {
		t.Fatal(err)
	}
	if sc.notifyOne(ctx, img, model.NotifNewTag, false) {
		t.Error("通知历史清空后不该重发同一版本")
	}

	// 仓库真的出了更高版本，照常提醒。
	img.LatestTag = "v3.8.0"
	if !sc.notifyOne(ctx, img, model.NotifNewTag, false) {
		t.Error("出现更高的版本时应重新推送")
	}
}

// 「稍后提醒」必须真的能再提一次。
//
// 抑制标记写在监控项上之后，稍后提醒就不能只改卡片状态了：
// 不清标记的话，下一轮会认为「这一版说过了」，卡片上那句
// 「下次巡检会重新推送」就成了空话。
func TestNotifyResumesAfterSnooze(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	sc := &Scanner{st: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	const ref, cid = "gdy666/lucky:v3.6.0", "c2"

	imgID, err := st.UpsertImage(&model.Image{
		Reference: ref, ContainerID: cid, ContainerName: "lucky",
		Origin: model.OriginContainer, LocalTag: "v3.6.0",
		LatestTag: "v3.7.0", Status: model.StatusNewTag,
	})
	if err != nil {
		t.Fatal(err)
	}
	img, err := st.FindImage(ref, cid)
	if err != nil {
		t.Fatal(err)
	}

	if !sc.notifyOne(ctx, img, model.NotifNewTag, false) {
		t.Fatal("第一轮应当推送提醒")
	}
	pending, err := st.FindPendingNotification(ref, cid)
	if err != nil {
		t.Fatalf("应能找到刚推送的提醒: %v", err)
	}

	// 用户在卡片上点「稍后提醒」：改状态 + 清标记。
	if err := st.SetNotificationCardState(pending.ID, model.CardSnoozed); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearImageNotified(imgID); err != nil {
		t.Fatal(err)
	}
	img, err = st.FindImage(ref, cid)
	if err != nil {
		t.Fatal(err)
	}

	if !sc.notifyOne(ctx, img, model.NotifNewTag, false) {
		t.Error("设为稍后之后，下一轮应当重新推送")
	}
}

// 用户点过「恢复提醒」之后，同一版本应当重新推送。
func TestNotifyResumesAfterIgnoredReset(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	sc := &Scanner{st: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	const ref, cid = "gdy666/lucky:v3.6.0", "c3"

	imgID, err := st.UpsertImage(&model.Image{
		Reference: ref, ContainerID: cid, ContainerName: "lucky",
		Origin: model.OriginContainer, LocalTag: "v3.6.0",
		LatestTag: "v3.7.0", Status: model.StatusNewTag,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetImageNotified(imgID, "v3.7.0", ""); err != nil {
		t.Fatal(err)
	}
	img, err := st.FindImage(ref, cid)
	if err != nil {
		t.Fatal(err)
	}
	if sc.notifyOne(ctx, img, model.NotifNewTag, false) {
		t.Fatal("已提醒过的版本不该重发")
	}

	// 「恢复提醒」＝ 解除忽略，标记必须一起清掉，否则按钮点了没反应。
	if err := st.SetImageIgnored(imgID, false); err != nil {
		t.Fatal(err)
	}
	img, err = st.FindImage(ref, cid)
	if err != nil {
		t.Fatal(err)
	}
	if img.NotifiedTag != "" || img.NotifiedDigest != "" {
		t.Fatalf("恢复提醒后标记应清空，得到 tag=%q digest=%q", img.NotifiedTag, img.NotifiedDigest)
	}
	if !sc.notifyOne(ctx, img, model.NotifNewTag, false) {
		t.Error("恢复提醒后应当重新推送")
	}
}

// 本地摘要必须真的写进 images 表。
//
// 这条路径以前只在内存里带着摘要：本机页面看着一切正常（通知是拿内存里的值拼的），
// 但远端节点是把这张表整份上报给 Hub 的，库里存着空串，卡片上就变成
// 「本地摘要：未知」—— 用户没法确认到底是哪一份镜像落后了。
func TestUpsertImagePersistsLocalDigest(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	sc := &Scanner{st: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	const ref = "ghcr.io/tashfeenahmed/freellmapi:latest"
	r, err := registry.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	c := dockerx.Container{ID: "c1", Name: "freellmapi", Image: ref, ImageID: "sha256:image1"}

	sc.upsertImage(nil, c, r, model.StatusUpdatePending, "sha256:localdigest")
	img, err := st.FindImage(ref, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if img.LocalDigest != "sha256:localdigest" {
		t.Fatalf("首轮落库的本地摘要丢了: %q", img.LocalDigest)
	}

	// 第二轮必须原样保留：UpsertImage 的冲突分支会用传入值覆盖这一列，
	// 少传一次就等于每轮巡检都把摘要抹掉一遍。
	sc.upsertImage(img, c, r, model.StatusUpdatePending, "sha256:localdigest")
	img, err = st.FindImage(ref, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if img.LocalDigest != "sha256:localdigest" {
		t.Fatalf("第二轮巡检把本地摘要覆盖成空: %q", img.LocalDigest)
	}
}

// 自升级期间会短暂出现 autodocker-agent-old（改名待删）和 autodocker-agent-upgrade（helper），
// 挨个判断「要不要排除」太容易漏，这里直接拿 selfupgrade 拼出来的名单去喂 skip：
// 名单一变，这条测试会先红，而不是等到用户截图上多出一个陌生容器。
func TestSkipSelfUpgradeContainers(t *testing.T) {
	self := "autodocker-agent"
	s := &Scanner{}
	opt := Options{
		SelfContainer:     self,
		ExcludeContainers: selfupgrade.AuxContainers(self),
	}
	for _, name := range selfupgrade.AuxContainers(self) {
		c := dockerx.Container{ID: "abc", Name: name, Image: "alpine:3.24"}
		if !s.skip(c, opt) {
			t.Errorf("%s 应被排除在监控之外", name)
		}
	}
	// 普通容器一个都不能误伤
	if c := (dockerx.Container{ID: "def", Name: "joplin", Image: "flamendes/joplin:latest"}); s.skip(c, opt) {
		t.Error("普通容器被误排除了")
	}
}

// 排除名单必须按**实际**容器名拼，不能照抄配置。
//
// 这就是 autodocker-agent-old 一直挂在节点列表里的原因：SelfContainer 的默认值
// 是 autodocker，而 Agent 装出来叫 autodocker-agent，于是拼出的是 autodocker-old，
// 跟列表里的名字差一个后缀，排除永远不生效。
func TestWithSelfExcludesUsesActualContainerName(t *testing.T) {
	opt := Options{
		SelfContainer:   "autodocker", // 默认值，与实际情况不符
		SelfContainerID: "abc123def456",
	}
	list := []dockerx.Container{
		{ID: "abc123def4567890aaaa", Name: "autodocker-agent", Image: "alpine:3.24"},
		{ID: "zzz", Name: "joplin", Image: "flamendes/joplin:latest"},
	}
	got := withSelfExcludes(opt, list)

	s := &Scanner{}
	old := dockerx.Container{ID: "stu", Name: "autodocker-agent-old", Image: "alpine:3.24"}
	if !s.skip(old, got) {
		t.Errorf("实际名字拼出来的 -old 没被排除: %v", got.ExcludeContainers)
	}
	helper := dockerx.Container{ID: "vwx", Name: "autodocker-agent-upgrade", Image: "alpine:3.24"}
	if !s.skip(helper, got) {
		t.Error("helper 容器没被排除")
	}
	// 别的容器一个都不能误伤
	if normal := (dockerx.Container{ID: "uvw", Name: "cloud-saver", Image: "jiangrui1994/cloudsaver:latest"}); s.skip(normal, got) {
		t.Error("普通容器被误排除了")
	}

	// 认不出自身时不该凭空多塞名字（返回原样，不 panic）
	if bare := withSelfExcludes(Options{SelfContainer: "autodocker"}, list); len(bare.ExcludeContainers) != 0 {
		t.Errorf("认不出自身时不该编造排除项: %v", bare.ExcludeContainers)
	}
}

// 同一个 compose 项目里的容器要合成一组，别的容器各走各的。
//
// 合并的依据是容器标签，不是名字前缀：自定义了 container_name 的项目
// 用前缀根本认不出来，最后还是一人一条。
func TestGroupHitsMergesComposeProject(t *testing.T) {
	hits := []updateHit{
		{img: &model.Image{ID: 1, ContainerID: "c1", ContainerName: "chaos", ComposeProject: "safeline"}, kind: model.NotifNewVersion},
		{img: &model.Image{ID: 2, ContainerID: "c2", ContainerName: "fvm", ComposeProject: "safeline"}, kind: model.NotifNewVersion},
		{img: &model.Image{ID: 3, ContainerID: "c3", ContainerName: "solo"}, kind: model.NotifNewVersion},
	}
	groups := groupHits(hits)
	if len(groups) != 2 {
		t.Fatalf("应分成「safeline 一组 + 单独容器一组」，实际 %d 组", len(groups))
	}
	if len(groups[0]) != 2 || groups[0][0].img.ContainerName != "chaos" {
		t.Errorf("同项目的两项应并到一组且保持顺序: %+v", groups[0])
	}
	if len(groups[1]) != 1 || groups[1][0].img.ContainerName != "solo" {
		t.Errorf("没有项目标签的容器应单独一组: %+v", groups[1])
	}
}

// 一次巡检里同项目的多个更新只落一条通知、只推一条消息。
func TestNotifyHitsCreatesOneGroupedNotification(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	sc := &Scanner{st: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var hits []updateHit
	for i, name := range []string{"chaos", "fvm", "mgt"} {
		id, err := st.UpsertImage(&model.Image{
			Reference:      "chaitin/safeline-" + name + ":9.4.3",
			ContainerID:    "c" + string(rune('1'+i)),
			ContainerName:  "safeline-" + name,
			ComposeProject: "safeline",
			Origin:         model.OriginContainer,
			LocalTag:       "9.4.3",
			RemoteDigest:   "sha256:new",
			Status:         model.StatusUpdatePending,
		})
		if err != nil {
			t.Fatal(err)
		}
		hits = append(hits, updateHit{
			img:  &model.Image{ID: id, Reference: "chaitin/safeline-" + name + ":9.4.3", ContainerID: "c" + string(rune('1'+i)), ContainerName: "safeline-" + name, ComposeProject: "safeline", LocalTag: "9.4.3", RemoteDigest: "sha256:new"},
			kind: model.NotifNewVersion,
		})
	}

	// 没有注入通知实现时只落库：正好用来验证合并结果。
	if n := sc.notifyHits(context.Background(), hits, false); n != 3 {
		t.Fatalf("三条都该计入提醒，实际 %d", n)
	}
	list, err := st.ListNotifications(false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("同项目应只落一条通知，实际 %d 条", len(list))
	}
	got := list[0]
	if got.MemberCount != 3 || len(got.Members) != 3 {
		t.Errorf("一条通知应覆盖 3 个容器，实际 count=%d members=%d", got.MemberCount, len(got.Members))
	}
	if got.ComposeProject != "safeline" {
		t.Errorf("应记下 compose 项目名，实际 %q", got.ComposeProject)
	}
	if !strings.Contains(got.Title, "safeline（3 个容器）") {
		t.Errorf("标题应写明项目与容器数，实际 %q", got.Title)
	}
}

// depends_on 标签里是「服务名:启动条件:是否必须」，解析只取服务名。
// 写错这一层，排序就会拿服务名去套容器名，一个都对不上、静默失效。
func TestParseComposeDeps(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"safeline-pg:service_started:false", []string{"safeline-pg"}},
		{
			"safeline-pg:service_started:false,safeline-mgt:service_healthy:true",
			[]string{"safeline-pg", "safeline-mgt"},
		},
		// 没有条件段的老写法，只有服务名
		{"db,redis", []string{"db", "redis"}},
		// 空项与重复项
		{" db , ,db,redis:service_started:true", []string{"db", "redis"}},
		// 只有冒号没有名字
		{":service_started", nil},
	}
	for _, c := range cases {
		got := parseComposeDeps(c.raw)
		if len(got) != len(c.want) {
			t.Fatalf("%q 解析成 %v，期望 %v", c.raw, got, c.want)
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Fatalf("%q 解析成 %v，期望 %v", c.raw, got, c.want)
			}
		}
	}
}

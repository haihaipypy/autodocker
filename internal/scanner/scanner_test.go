package scanner

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/haihaipypy/autodocker/internal/model"
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
	if sc.notify(ctx, img, model.NotifNewVersion, false) {
		t.Error("已忽略的这一版不应再次推送通知")
	}

	// 仓库又推了新内容（摘要变了）→ 照常推送
	img.RemoteDigest = "sha256:newer"
	if !sc.notify(ctx, img, model.NotifNewVersion, false) {
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

	if !sc.notify(ctx, img, model.NotifNewTag, false) {
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
	if sc.notify(ctx, img, model.NotifNewTag, false) {
		t.Error("通知历史清空后不该重发同一版本")
	}

	// 仓库真的出了更高版本，照常提醒。
	img.LatestTag = "v3.8.0"
	if !sc.notify(ctx, img, model.NotifNewTag, false) {
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

	if !sc.notify(ctx, img, model.NotifNewTag, false) {
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

	if !sc.notify(ctx, img, model.NotifNewTag, false) {
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
	if sc.notify(ctx, img, model.NotifNewTag, false) {
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
	if !sc.notify(ctx, img, model.NotifNewTag, false) {
		t.Error("恢复提醒后应当重新推送")
	}
}

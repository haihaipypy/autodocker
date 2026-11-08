package store

import (
	"path/filepath"
	"testing"

	"github.com/haihaipypy/autodocker/internal/model"
)

// 首页那排数字必须只数**容器**。
//
// 曾经把全部镜像都算进去，结果总数 15、而「有更新 + 可选更新 + 已是最新」
// 只有 8 —— 中间那 7 条是没人用的悬空层和纯远端监控项。用户看到的是
// 「统计坏了」，其实是口径混了两类东西。这条测试就是钉住这个口径。
func TestCountImagesByStatusFiltersByOrigin(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	seed := []*model.Image{
		// 三个容器：一个待更新、一个已最新、一个检测失败
		{Reference: "nginx:1.25", ContainerID: "c1", ContainerName: "web", Origin: model.OriginContainer, Status: model.StatusUpdatePending},
		{Reference: "redis:7", ContainerID: "c2", ContainerName: "cache", Origin: model.OriginContainer, Status: model.StatusUpToDate},
		{Reference: "127.0.0.1:5000/myapp:latest", ContainerID: "c3", ContainerName: "app", Origin: model.OriginContainer, Status: model.StatusUnknown},
		// 两个本机镜像（没有容器在用）：一个悬空层、一个只是没用起来的新版本
		{Reference: "sha256:aaaa", Origin: model.OriginLocal, Dangling: true, Status: model.StatusUnknown},
		{Reference: "registry:3.1.2", Origin: model.OriginLocal, Status: model.StatusNewTag},
		// 一个纯远端监控项
		{Reference: "ghcr.io/foo/bar:latest", Origin: model.OriginWatch, Status: model.StatusUnknown},
	}
	for _, img := range seed {
		if _, err := s.UpsertImage(img); err != nil {
			t.Fatalf("写入 %s 失败: %v", img.Reference, err)
		}
	}

	all, err := s.CountImagesByStatus()
	if err != nil {
		t.Fatal(err)
	}
	if got := total(all); got != len(seed) {
		t.Errorf("不带 origin 时应统计全部 %d 条，得到 %d（%v）", len(seed), got, all)
	}

	got, err := s.CountImagesByStatus(model.OriginContainer)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		string(model.StatusUpdatePending): 1,
		string(model.StatusUpToDate):      1,
		string(model.StatusUnknown):       1,
	}
	if len(got) != len(want) {
		t.Fatalf("只数容器时应得到 %v，实际 %v", want, got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("状态 %s 应为 %d，实际 %d", k, v, got[k])
		}
	}
	// 本机镜像与纯远端项不能被算进来 —— 这正是上一版数字对不上的原因
	if got[string(model.StatusNewTag)] != 0 {
		t.Errorf("没人用的镜像出新版不应计入首页「可选更新」，实际 %d", got[string(model.StatusNewTag)])
	}

	// 多个 origin 也要能一起统计
	both, err := s.CountImagesByStatus(model.OriginContainer, model.OriginWatch)
	if err != nil {
		t.Fatal(err)
	}
	if total(both) != 4 {
		t.Errorf("容器 + 纯远端应为 4 条，实际 %d（%v）", total(both), both)
	}
}

func total(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// 容器的镜像引用换了写法，不能留下两行。
//
// 唯一键是 (reference, container_id)，容器还在的时候 DeleteImagesMissing
// 管不到旧行。实机出现过：同一个 ad-myapp 容器，一行 reference 是
// `127.0.0.1:5000/myapp:latest`，另一行是裸 digest，容器页上同一个容器出现两次。
func TestContainerUpsertRemovesStaleReferenceRow(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	const cid = "de6e3eefb61f"
	// 先按 tag 记一条
	if _, err := s.UpsertImage(&model.Image{
		Reference: "127.0.0.1:5000/myapp:latest", ContainerID: cid,
		ContainerName: "ad-myapp", Origin: model.OriginContainer, Status: model.StatusUnknown,
	}); err != nil {
		t.Fatal(err)
	}
	// 同一个容器，Docker 这次报的是裸 digest
	if _, err := s.UpsertImage(&model.Image{
		Reference: "sha256:6a9d11b53cada658", ContainerID: cid,
		ContainerName: "ad-myapp", Origin: model.OriginContainer, Status: model.StatusUnknown,
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListImages(model.OriginContainer)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("同一个容器只应留一行，实际 %d 行", len(rows))
	}
	if rows[0].Reference != "sha256:6a9d11b53cada658" {
		t.Errorf("应保留最新观测到的引用，实际 %q", rows[0].Reference)
	}

	// 另一个容器用同名 tag 是正常情况，不能被误删
	if _, err := s.UpsertImage(&model.Image{
		Reference: "127.0.0.1:5000/myapp:latest", ContainerID: "other-cid",
		ContainerName: "ad-myapp2", Origin: model.OriginContainer, Status: model.StatusUnknown,
	}); err != nil {
		t.Fatal(err)
	}
	// 纯远端项（container_id 为空）更不该被牵连
	if _, err := s.UpsertImage(&model.Image{
		Reference: "nginx:latest", Origin: model.OriginWatch, Status: model.StatusUnknown,
	}); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ListImages(model.OriginContainer)
	if len(rows) != 2 {
		t.Errorf("两个容器应各留一行，实际 %d 行", len(rows))
	}
	all, _ := s.ListImages()
	if len(all) != 3 {
		t.Errorf("加上纯远端项应为 3 行，实际 %d 行", len(all))
	}
}

// 「忽略本版本」必须真的落到监控项上，而且不能被下一轮扫描冲掉。
//
// 旧写法只改了通知卡片的 state，扫描端去重只看「有没有待处理通知」，
// 于是忽略完下一轮又新建一张 —— 卡片上那句「同一个版本不会再次提醒」是空话。
// UpsertImage 是扫描的落库入口，ON CONFLICT 分支若把 ignored_digest 一起刷成
// excluded 值，这个标记每轮都会被清空，抑制逻辑就形同虚设。这里钉住它。
func TestIgnoredDigestSurvivesUpsertAndClearsOnRestore(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	const ref, cid = "127.0.0.1:5000/myapp:latest", "c1"
	id, err := s.UpsertImage(&model.Image{
		Reference: ref, ContainerID: cid, ContainerName: "app",
		Origin: model.OriginContainer, Status: model.StatusUpdatePending,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 用户在飞书卡片上点了「忽略本版本」
	if err := s.SetImageIgnoredDigest(id, "sha256:new"); err != nil {
		t.Fatal(err)
	}

	// 下一轮扫描：同一容器再 Upsert 一次，标记必须原样留着
	if _, err := s.UpsertImage(&model.Image{
		Reference: ref, ContainerID: cid, ContainerName: "app",
		Origin: model.OriginContainer, Status: model.StatusUpdatePending,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.FindImage(ref, cid)
	if err != nil {
		t.Fatal(err)
	}
	if got.IgnoredDigest != "sha256:new" {
		t.Fatalf("扫描落库后 ignored_digest 应保留为 sha256:new，实际 %q", got.IgnoredDigest)
	}

	// 恢复监控（点「恢复提醒」走的就是 SetImageIgnored(id,false)）要连标记一起清掉，
	// 否则用户点了恢复却还是收不到提醒。
	if err := s.SetImageIgnored(id, false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.FindImage(ref, cid)
	if got.IgnoredDigest != "" {
		t.Errorf("恢复监控后 ignored_digest 应清空，实际 %q", got.IgnoredDigest)
	}
	if got.Ignored {
		t.Errorf("恢复监控后 ignored 应为 false")
	}
}

// 升 tag 重建之后，这一行的引用是**旧的**，容器号是**新的**。
//
// 谁在这个时刻还用 (新引用, 旧容器号) 去查，必然扑空 —— 而扑空不会报错，
// 只会让「更新成功后刷新状态」整段被跳过，页面永远停在
// 「当前 v3.6.0 / 可更新 v3.7.0」。所以按容器号查这条路径必须存在。
func TestFindImageByContainerAfterTagUpgrade(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	img := &model.Image{
		Reference:     "gdy666/lucky:v3.6.0",
		ContainerID:   "old-cid",
		ContainerName: "lucky",
		Origin:        model.OriginContainer,
		LocalTag:      "v3.6.0",
		LatestTag:     "v3.7.0",
		Status:        model.StatusNewTag,
		Mode:          model.ModeAuto,
	}
	id, err := s.UpsertImage(img)
	if err != nil {
		t.Fatalf("写入监控项失败: %v", err)
	}

	// 精确查（新引用 + 旧容器号）扑空：这正是原来的写法，也是这个 bug 的成因。
	if _, err := s.FindImage("gdy666/lucky:v3.7.0", "old-cid"); err == nil {
		t.Fatal("新引用配旧容器号本不该查到，查到说明唯一键变了")
	}
	got, err := s.FindImageByContainer("old-cid")
	if err != nil {
		t.Fatalf("按容器号应查到：%v", err)
	}
	if got.ID != id {
		t.Fatalf("按容器号查到的是另一行：%d != %d", got.ID, id)
	}

	// 重建完成后对齐：引用、本地 tag、容器号一起换。
	if err := s.UpdateImageAfterRebuild(id, "gdy666/lucky:v3.7.0", "sha256:new",
		"new-cid", "lucky", "sha256:newdigest", "v3.7.0"); err != nil {
		t.Fatalf("对齐监控项失败: %v", err)
	}
	after, err := s.FindImageByContainer("new-cid")
	if err != nil {
		t.Fatalf("新容器号应能查到：%v", err)
	}
	if after.Reference != "gdy666/lucky:v3.7.0" || after.LocalTag != "v3.7.0" {
		t.Fatalf("引用与本地 tag 没跟上，得到 %s / %s", after.Reference, after.LocalTag)
	}
	// 下一轮扫描就是按 (容器当前镜像, 容器号) 找，必须能命中同一行，
	// 否则会多插一条记录，列表里同一个容器出现两次。
	if _, err := s.FindImage("gdy666/lucky:v3.7.0", "new-cid"); err != nil {
		t.Fatalf("下一轮扫描应命中同一行：%v", err)
	}
}

// 容器编号失效后，那一行必须能被重新挂到真容器上。
//
// 场景：某次重建成功却没把新编号写回（1.5.7 之前），库里留着的是已经被删掉的
// 容器编号。之后每次点更新都拿着死编号去 inspect，一律 404，用户只能删掉监控项
// 重加、历史一起丢。修好这一列，下一次就能正常更新。
func TestUpdateImageContainerRemapsStaleID(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	id, err := s.UpsertImage(&model.Image{
		Reference:     "gdy666/lucky:v3.6.0",
		ContainerID:   "dead-cid",
		ContainerName: "lucky",
		Origin:        model.OriginContainer,
		LocalTag:      "v3.6.0",
		Status:        model.StatusNewTag,
	})
	if err != nil {
		t.Fatalf("写入监控项失败: %v", err)
	}

	if err := s.UpdateImageContainer(id, "live-cid", "lucky"); err != nil {
		t.Fatalf("重挂容器编号失败: %v", err)
	}

	if _, err := s.FindImageByContainer("dead-cid"); err == nil {
		t.Fatal("旧编号不该再查得到")
	}
	got, err := s.FindImageByContainer("live-cid")
	if err != nil {
		t.Fatalf("新编号应查得到：%v", err)
	}
	if got.ID != id || got.Reference != "gdy666/lucky:v3.6.0" {
		t.Fatalf("重挂后不该动引用，得到 id=%d ref=%s", got.ID, got.Reference)
	}
}

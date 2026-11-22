package hub

import (
	"path/filepath"
	"testing"

	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/store"
)

func newTestClient(t *testing.T) (*Client, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Client{st: st}, st
}

// updateTarget 决定「这次到底拉哪个引用」，弄错的后果是用户点了更新
// 却只把同一个旧镜像又拉了一遍，或者把 tag 从 1.5 降回 1.2。
//
// 判定留在 Agent 上 —— Hub 不知道本地到底跑的是哪个 tag。
func TestUpdateTarget(t *testing.T) {
	c, st := newTestClient(t)

	cases := []struct {
		name       string
		ref        string
		localTag   string
		latestTag  string
		wantImage  string
		wantTarget string
	}{
		{
			name: "同 tag 内容变了：拉原引用",
			ref:  "nginx:latest", localTag: "latest",
			wantImage: "nginx:latest", wantTarget: "nginx:latest",
		},
		{
			name: "仓库出了更高版本：换成新 tag",
			ref:  "ghcr.io/freeok/sonovel:v3.6.0", localTag: "v3.6.0", latestTag: "v3.7.0",
			wantImage:  "ghcr.io/freeok/sonovel:v3.6.0",
			wantTarget: "ghcr.io/freeok/sonovel:v3.7.0",
		},
		{
			name: "更高的 tag 反而不如当前：不降级",
			ref:  "app:1.5", localTag: "1.5", latestTag: "1.2",
			wantImage: "app:1.5", wantTarget: "app:1.5",
		},
	}

	for i, tc := range cases {
		containerID := "c" + string(rune('a'+i))
		if _, err := st.UpsertImage(&model.Image{
			Reference:     tc.ref,
			ContainerID:   containerID,
			ContainerName: "svc",
			Origin:        model.OriginContainer,
			LocalTag:      tc.localTag,
			LatestTag:     tc.latestTag,
		}); err != nil {
			t.Fatalf("%s：写入监控项失败: %v", tc.name, err)
		}

		gotImage, gotTarget := c.updateTarget(containerID, "svc")
		if gotImage != tc.wantImage {
			t.Errorf("%s：原始引用应为 %q，实际 %q", tc.name, tc.wantImage, gotImage)
		}
		if gotTarget != tc.wantTarget {
			t.Errorf("%s：目标引用应为 %q，实际 %q", tc.name, tc.wantTarget, gotTarget)
		}
	}
}

// 容器编号在库里找不到（刚被删、或者快照过期）时不能瞎猜一个引用去拉，
// 宁可返回空让上层直接报错。
func TestUpdateTargetUnknownContainer(t *testing.T) {
	c, _ := newTestClient(t)
	if ref, target := c.updateTarget("does-not-exist", ""); ref != "" || target != "" {
		t.Errorf("未知容器应返回空引用，实际 %q / %q", ref, target)
	}
}

// 编号过期、名字还在时，按名字把引用找回来。
//
// 容器被重建过之后编号就变了，Hub 手上的编号可能比本地记录还新；
// 只认编号会推不出引用，最后拿空字符串去拉镜像，报错还看不出卡在哪一步。
func TestUpdateTargetFallsBackToContainerName(t *testing.T) {
	c, st := newTestClient(t)
	if _, err := st.UpsertImage(&model.Image{
		Reference:     "ghcr.io/freeok/sonovel:v3.6.0",
		ContainerID:   "old-id",
		ContainerName: "sonovel",
		Origin:        model.OriginContainer,
		LocalTag:      "v3.6.0",
		LatestTag:     "v3.7.0",
	}); err != nil {
		t.Fatal(err)
	}

	ref, target := c.updateTarget("new-id", "sonovel")
	if ref != "ghcr.io/freeok/sonovel:v3.6.0" || target != "ghcr.io/freeok/sonovel:v3.7.0" {
		t.Errorf("按名字应能推出目标引用，实际 %q / %q", ref, target)
	}
}

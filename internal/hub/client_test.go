package hub

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
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

// Hub 下发的加速地址要真的换到检测用的客户端上，并且记住。
//
// 这是「设置页改了、节点页还是整列未知」那条问题的出口：只在进程启动时
// 读一次环境变量的做法，用户改完 Hub 设置永远等不到生效。
func TestApplyMirrorsSwapsAndPersists(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var applied [][]string
	c := New(Config{
		BaseMirrors: []string{"base.local"},
		OnMirrors:   func(m []string) { applied = append(applied, m) },
	}, nil, st, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	// 一份新列表：应当整体替换并落盘。
	c.applyMirrors(&[]string{"a.local", "b.local"})
	if len(applied) != 1 || len(applied[0]) != 2 || applied[0][0] != "a.local" {
		t.Fatalf("新列表没应用下去：%v", applied)
	}
	if got := c.mirrorList(); len(got) != 2 || got[1] != "b.local" {
		t.Fatalf("内存里没记住：%v", got)
	}
	raw, ok, err := st.GetSetting(MirrorsSettingKey)
	if err != nil || !ok {
		t.Fatalf("没落盘：ok=%v err=%v", ok, err)
	}
	if !strings.Contains(raw, "a.local") || !strings.Contains(raw, "b.local") {
		t.Fatalf("落盘内容不对：%s", raw)
	}

	// 同一份再来一次：不重复应用（每次重连都刷一遍日志没有意义）。
	c.applyMirrors(&[]string{"a.local", "b.local"})
	if len(applied) != 1 {
		t.Fatalf("相同的列表不该重复应用，实际调用了 %d 次", len(applied))
	}

	// 空列表 = Hub 说「别用它那套」，回到这台机器自己的配置。
	c.applyMirrors(&[]string{})
	if len(applied) != 2 || len(applied[1]) != 1 || applied[1][0] != "base.local" {
		t.Fatalf("空列表应回退到本机配置，实际 %v", applied)
	}

	// nil = 没这条指令，什么都不该发生。
	before := len(applied)
	c.applyMirrors(nil)
	if len(applied) != before {
		t.Fatalf("nil 不该触发应用，实际多了一次：%v", applied)
	}
}

// 启动时优先用上次下发的那份，而不是本机默认。
//
// Hub 一时连不上时，节点仍要按用户配好的源检测 —— 退回内置默认
// 在境内基本等于整列「检查异常」。
func TestInitialMirrorsWinOverBase(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	c := New(Config{
		BaseMirrors:    []string{"base.local"},
		InitialMirrors: []string{"saved.local"},
	}, nil, st, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if got := c.mirrorList(); len(got) != 1 || got[0] != "saved.local" {
		t.Fatalf("启动时应当用上次下发的那份，实际 %v", got)
	}
}

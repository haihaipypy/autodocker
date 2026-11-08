package notify

import (
	"path/filepath"
	"testing"

	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/store"
)

// TargetRef 决定了「点了更新按钮之后到底去拉哪个镜像」。
// 判错就是空操作：跑完一圈，容器还在旧版本上。
func TestTargetRef(t *testing.T) {
	cases := []struct {
		name string
		n    model.Notification
		want string
	}{
		{
			name: "浮动 tag 内容变更：仍拉原引用",
			n: model.Notification{
				ImageRef: "nginx:latest",
				Kind:     model.NotifNewVersion,
			},
			want: "nginx:latest",
		},
		{
			name: "锁定版本发现新 tag：必须换成新版本号",
			n: model.Notification{
				ImageRef:  "nginx:1.25",
				LocalTag:  "1.25",
				LatestTag: "1.27",
				Kind:      model.NotifNewTag,
			},
			want: "nginx:1.27",
		},
		{
			name: "带仓库路径的锁定版本",
			n: model.Notification{
				ImageRef:  "joplin/server:2.13.5",
				LocalTag:  "2.13.5",
				LatestTag: "2.13.6",
				Kind:      model.NotifNewTag,
			},
			want: "joplin/server:2.13.6",
		},
		{
			name: "私有仓库的锁定版本",
			n: model.Notification{
				ImageRef:  "ghcr.io/foo/bar:1.2.3",
				LocalTag:  "1.2.3",
				LatestTag: "1.3.0",
				Kind:      model.NotifNewTag,
			},
			want: "ghcr.io/foo/bar:1.3.0",
		},
		{
			name: "LatestTag 与当前 tag 相同：不折腾，用原引用",
			n: model.Notification{
				ImageRef:  "nginx:1.25",
				LocalTag:  "1.25",
				LatestTag: "1.25",
			},
			want: "nginx:1.25",
		},
		{
			name: "页面直接更新（Kind 未标）也能靠 LatestTag 判出来",
			n: model.Notification{
				ImageRef:  "redis:7.2",
				LocalTag:  "7.2",
				LatestTag: "7.4",
			},
			want: "redis:7.4",
		},
		{
			name: "默认 tag 缺省写法",
			n: model.Notification{
				ImageRef:  "alpine",
				LocalTag:  "latest",
				LatestTag: "3.21",
			},
			want: "alpine:3.21",
		},
		{
			name: "无法解析时保持原样，不做猜测",
			n: model.Notification{
				ImageRef:  "",
				LatestTag: "1.0",
			},
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TargetRef(&c.n)
			if got != c.want {
				t.Fatalf("TargetRef = %q, 期望 %q", got, c.want)
			}
		})
	}
}

// 卡片上那个「更新」必须真的更新到最新版本。
//
// 通知可能是几天前「同一个 tag 内容变了」那版发出去的，那时 LatestTag 还是空的；
// 等用户翻出来点，监控项里早记着更高版本了。只看通知就会照着旧 tag 重建一遍 ——
// 耗时几十秒、卡片报成功，页面上的「可更新」一步没动（这正是 1.5.6 手测看到的）。
func TestUpdateTargetPrefersStoredLatestTag(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	svc := &Service{st: s}

	// 老通知：同一 tag 内容变更，通知里没有更高版本。
	n := &model.Notification{
		ImageRef:      "gdy666/lucky:v3.6.0",
		ContainerID:   "c1",
		ContainerName: "lucky",
		Kind:          model.NotifNewVersion,
	}
	if got := svc.updateTarget(n); got != "gdy666/lucky:v3.6.0" {
		t.Fatalf("没有更高版本时不该改目标，得到 %q", got)
	}

	// 监控项后来发现了 v3.7.0（扫描写的，通知里不会更新）。
	if _, err := s.UpsertImage(&model.Image{
		Reference:     "gdy666/lucky:v3.6.0",
		ContainerID:   "c1",
		ContainerName: "lucky",
		Origin:        model.OriginContainer,
		LocalTag:      "v3.6.0",
		LatestTag:     "v3.7.0",
		Status:        model.StatusNewTag,
	}); err != nil {
		t.Fatal(err)
	}
	if got := svc.updateTarget(n); got != "gdy666/lucky:v3.7.0" {
		t.Fatalf("应跟着监控项里的更高版本走，得到 %q", got)
	}

	// 同一版本写法不同（3.7 与 v3.7.0）不该被当成新版本来回折腾。
	if _, err := s.UpsertImage(&model.Image{
		Reference:   "gdy666/lucky:3.7",
		ContainerID: "c2",
		Origin:      model.OriginContainer,
		LocalTag:    "3.7",
		LatestTag:   "v3.7.0",
		Status:      model.StatusNewTag,
	}); err != nil {
		t.Fatal(err)
	}
	n2 := &model.Notification{ImageRef: "gdy666/lucky:3.7", ContainerID: "c2"}
	if got := svc.updateTarget(n2); got != "gdy666/lucky:3.7" {
		t.Fatalf("同版本不同写法应保持原引用，得到 %q", got)
	}
}

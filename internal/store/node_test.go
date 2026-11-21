package store

import (
	"path/filepath"
	"testing"

	"github.com/haihaipypy/autodocker/internal/model"
)

func newTestNodeStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// 令牌是进 Hub 的唯一凭据，换掉之后旧的一定要立刻失效 ——
// 否则「重发命令」这个动作就只是心理安慰。
func TestNodeTokenLifecycle(t *testing.T) {
	s := newTestNodeStore(t)

	id, err := s.CreateNode(&model.Node{Name: "群晖", HubURL: "http://192.168.2.10:19999"}, "hash-old")
	if err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}

	n, err := s.FindNodeByTokenHash("hash-old")
	if err != nil {
		t.Fatalf("按令牌查节点失败: %v", err)
	}
	if n.ID != id || n.Name != "群晖" {
		t.Fatalf("查回的节点不对: %+v", n)
	}
	if n.Status != model.NodeNever {
		t.Errorf("新节点状态应为 never，实际 %s", n.Status)
	}

	if _, err := s.FindNodeByTokenHash("hash-unknown"); err != ErrNotFound {
		t.Errorf("未知令牌应返回 ErrNotFound，实际 %v", err)
	}
	if _, err := s.FindNodeByTokenHash(""); err != ErrNotFound {
		t.Errorf("空令牌应返回 ErrNotFound，实际 %v", err)
	}

	if err := s.RotateNodeToken(id, "hash-new"); err != nil {
		t.Fatalf("换令牌失败: %v", err)
	}
	if _, err := s.FindNodeByTokenHash("hash-old"); err != ErrNotFound {
		t.Error("旧令牌换掉之后仍然可用")
	}
	if _, err := s.FindNodeByTokenHash("hash-new"); err != nil {
		t.Errorf("新令牌不可用: %v", err)
	}
}

// 上报是整份替换，不是合并。这条钉住的是「容器被删掉后不会在页面上赖着」。
func TestReplaceNodeContainersIsFullReplace(t *testing.T) {
	s := newTestNodeStore(t)
	id, err := s.CreateNode(&model.Node{Name: "vps"}, "h")
	if err != nil {
		t.Fatal(err)
	}

	first := []model.NodeContainer{
		{NodeID: id, ContainerID: "c1", ContainerName: "web", ImageRef: "nginx:latest"},
		{NodeID: id, ContainerID: "c2", ContainerName: "db", ImageRef: "postgres:16"},
		{NodeID: id, ContainerID: "c3", ContainerName: "cache", ImageRef: "redis:7"},
	}
	if err := s.ReplaceNodeContainers(id, first); err != nil {
		t.Fatalf("首轮写入失败: %v", err)
	}

	second := []model.NodeContainer{
		{NodeID: id, ContainerID: "c1", ContainerName: "web", ImageRef: "nginx:latest", LatestTag: "1.27"},
	}
	if err := s.ReplaceNodeContainers(id, second); err != nil {
		t.Fatalf("第二轮写入失败: %v", err)
	}

	got, err := s.ListNodeContainers(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("整份替换后应只剩 1 条，实际 %d 条", len(got))
	}
	if got[0].ContainerID != "c1" || got[0].LatestTag != "1.27" {
		t.Errorf("留下的记录内容不对: %+v", got[0])
	}
}

// 一份上报里可以有多条「没有容器编号」的镜像维度条目。
// 以前按 (node_id, container_id) 建了唯一索引，第二条就把整份快照写崩，
// 节点页永远显示 0 个容器 —— 这条钉住的是那个回归。
func TestReplaceNodeContainersAllowsManyImageRows(t *testing.T) {
	s := newTestNodeStore(t)
	id, err := s.CreateNode(&model.Node{Name: "nas"}, "h")
	if err != nil {
		t.Fatal(err)
	}
	list := []model.NodeContainer{
		{NodeID: id, ContainerID: "c1", Origin: model.OriginContainer},
		{NodeID: id, ContainerID: "", ImageRef: "busybox:latest", Origin: model.OriginLocal},
		{NodeID: id, ContainerID: "", ImageRef: "alpine:3.24", Origin: model.OriginLocal},
		{NodeID: id, ContainerID: "", ImageRef: "redis:7", Origin: model.OriginWatch},
	}
	if err := s.ReplaceNodeContainers(id, list); err != nil {
		t.Fatalf("含多条镜像维度条目时写入失败: %v", err)
	}
	got, err := s.ListNodeContainers(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(list) {
		t.Errorf("应写入 %d 条，实际 %d 条", len(list), len(got))
	}
}

// 节点列表上的两个数字：容器数只数容器维度，待更新数把两种更新状态都算上。
func TestListNodesCounts(t *testing.T) {
	s := newTestNodeStore(t)
	id, err := s.CreateNode(&model.Node{Name: "nas"}, "h")
	if err != nil {
		t.Fatal(err)
	}
	list := []model.NodeContainer{
		{NodeID: id, ContainerID: "c1", Origin: model.OriginContainer, Status: model.StatusUpdatePending},
		{NodeID: id, ContainerID: "c2", Origin: model.OriginContainer, Status: model.StatusNewTag},
		{NodeID: id, ContainerID: "c3", Origin: model.OriginContainer, Status: model.StatusUpToDate},
		// 纯远端盯梢项不算容器
		{NodeID: id, ContainerID: "", ImageRef: "nginx:latest", Origin: model.OriginWatch, Status: model.StatusUpdatePending},
		// 镜像维度（本机闲置镜像）也不算容器 —— 老版本 Agent 会把它送上来
		{NodeID: id, ContainerID: "", ImageRef: "busybox:latest", Origin: model.OriginLocal, Status: model.StatusUpToDate},
	}
	if err := s.ReplaceNodeContainers(id, list); err != nil {
		t.Fatal(err)
	}

	nodes, err := s.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("应有 1 个节点，实际 %d", len(nodes))
	}
	if nodes[0].ContainerCount != 3 {
		t.Errorf("容器数应为 3，实际 %d", nodes[0].ContainerCount)
	}
	if nodes[0].UpdateCount != 2 {
		t.Errorf("待更新数应为 2，实际 %d", nodes[0].UpdateCount)
	}
}

// 「连上了」和「交过卷」是两件事：只有真正收过快照才盖 last_report_at，
// 页面靠它区分「巡检中」和「确实没有容器」。
func TestSetNodeReportedMarksFirstSnapshot(t *testing.T) {
	s := newTestNodeStore(t)
	id, err := s.CreateNode(&model.Node{Name: "nas"}, "h")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNodeHello(id, "host", "Linux", "amd64", "1.7.5", 1); err != nil {
		t.Fatal(err)
	}
	n, err := s.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	if !n.LastReportAt.IsZero() {
		t.Errorf("还没收到上报就不该有 last_report_at，实际 %v", n.LastReportAt)
	}

	if err := s.SetNodeReported(id); err != nil {
		t.Fatalf("记录上报时间失败: %v", err)
	}
	n, _ = s.GetNode(id)
	if n.LastReportAt.IsZero() {
		t.Error("收到上报后 last_report_at 仍为空")
	}
	if n.Status != model.NodeOnline {
		t.Errorf("收到上报应视为在线，实际 %s", n.Status)
	}
}

func TestDeleteNodeRemovesSnapshots(t *testing.T) {
	s := newTestNodeStore(t)
	id, err := s.CreateNode(&model.Node{Name: "gone"}, "h")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceNodeContainers(id, []model.NodeContainer{
		{NodeID: id, ContainerID: "c1", Origin: model.OriginContainer},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNode(id); err != nil {
		t.Fatalf("删除节点失败: %v", err)
	}
	if _, err := s.GetNode(id); err != ErrNotFound {
		t.Errorf("删除后仍能查到节点: %v", err)
	}
	list, err := s.ListNodeContainers(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("节点删除后快照应一并清掉，实际剩 %d 条", len(list))
	}
}

// 进程重启后，库里留着的「在线」都是假的 —— 必须全部按离线处理，
// 否则用户会看到一堆早就断了的节点显示绿点。
func TestMarkAllNodesOffline(t *testing.T) {
	s := newTestNodeStore(t)
	id, _ := s.CreateNode(&model.Node{Name: "a"}, "h")
	if err := s.UpdateNodeHello(id, "host", "Linux", "amd64", "1.7.0", 1); err != nil {
		t.Fatal(err)
	}
	n, _ := s.GetNode(id)
	if n.Status != model.NodeOnline {
		t.Fatalf("注册后应为在线，实际 %s", n.Status)
	}
	if err := s.MarkAllNodesOffline(); err != nil {
		t.Fatal(err)
	}
	n, _ = s.GetNode(id)
	if n.Status != model.NodeOffline {
		t.Errorf("重启后应标为离线，实际 %s", n.Status)
	}
}

package store

import (
	"database/sql"
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

// 首页「有更新」要把所有节点加起来，所以汇总的口径必须与节点卡片一致：
// 只数容器维度、两种更新状态都算，纯远端盯梢与镜像维度条目一律不算。
//
// 这条断言同时还守着「多个节点相加」这件事 —— 最容易出错的写法是只取第一台。
func TestNodeUpdateTotals(t *testing.T) {
	s := newTestNodeStore(t)

	// 没有节点时必须是 0 而不是报错：新装的人一次都还没接过节点。
	c, u, err := s.NodeUpdateTotals()
	if err != nil {
		t.Fatal(err)
	}
	if c != 0 || u != 0 {
		t.Fatalf("空库应返回 0/0，实际 %d/%d", c, u)
	}

	a, err := s.CreateNode(&model.Node{Name: "nas-a"}, "ha")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateNode(&model.Node{Name: "nas-b"}, "hb")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceNodeContainers(a, []model.NodeContainer{
		{NodeID: a, ContainerID: "c1", Origin: model.OriginContainer, Status: model.StatusUpdatePending},
		{NodeID: a, ContainerID: "c2", Origin: model.OriginContainer, Status: model.StatusNewTag},
		{NodeID: a, ContainerID: "c3", Origin: model.OriginContainer, Status: model.StatusUpToDate},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceNodeContainers(b, []model.NodeContainer{
		{NodeID: b, ContainerID: "d1", Origin: model.OriginContainer, Status: model.StatusUpdatePending},
		// 这两条不该被算进任何一个数
		{NodeID: b, ContainerID: "", ImageRef: "nginx:latest", Origin: model.OriginWatch, Status: model.StatusUpdatePending},
		{NodeID: b, ContainerID: "", ImageRef: "busybox:latest", Origin: model.OriginLocal, Status: model.StatusUpToDate},
	}); err != nil {
		t.Fatal(err)
	}

	c, u, err = s.NodeUpdateTotals()
	if err != nil {
		t.Fatal(err)
	}
	if c != 4 {
		t.Errorf("两个节点共 4 个容器，实际 %d", c)
	}
	if u != 3 {
		t.Errorf("两个节点共 3 个待更新，实际 %d", u)
	}
}

// 编号过期时要能按容器名找回同一个容器。
//
// 容器一被重建编号就变了，而提醒里存的是发提醒那天的编号。按名字兜底，
// 用户在提醒上点「重试」才可能真的动起来，而不是永远「找不到该容器」。
func TestFindNodeContainerByName(t *testing.T) {
	s := newTestNodeStore(t)
	id, err := s.CreateNode(&model.Node{Name: "1panel"}, "h")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceNodeContainers(id, []model.NodeContainer{
		{NodeID: id, ContainerID: "new-id", ContainerName: "freellmapi-1", ImageRef: "ghcr.io/x/y:latest"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.FindNodeContainer(id, "old-id"); err != ErrNotFound {
		t.Errorf("旧编号应查不到，实际 %v", err)
	}
	c, err := s.FindNodeContainerByName(id, "freellmapi-1")
	if err != nil {
		t.Fatalf("按名字应能找回: %v", err)
	}
	if c.ContainerID != "new-id" {
		t.Errorf("应返回当前那个实例的编号，实际 %q", c.ContainerID)
	}
	// 空名字不能撞上任何一行（镜像维度那些条目的名字是空的）
	if _, err := s.FindNodeContainerByName(id, ""); err != ErrNotFound {
		t.Errorf("空名字不该匹配到任何容器，实际 %v", err)
	}
}

// 老库升级必须把 compose_project 补到 node_containers 上。
//
// 这一条钉的是 1.10.0 的翻车现场：列补到了 nodes 表上，node_containers 一个字段都没加，
// 结果老库升级后节点页整块读不出来，报 no such column: compose_project。
// 建表语句对已存在的表不会补列，所以只能靠 ALTER，而补错表的表名没人看得出来。
func TestMigrateAddsComposeProjectToNodeContainers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	oldSchema := []string{
		`CREATE TABLE nodes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL DEFAULT '',
			hub_url TEXT NOT NULL DEFAULT '',
			note TEXT NOT NULL DEFAULT '',
			token_hash TEXT NOT NULL DEFAULT '',
			hostname TEXT NOT NULL DEFAULT '',
			os TEXT NOT NULL DEFAULT '',
			arch TEXT NOT NULL DEFAULT '',
			agent_version TEXT NOT NULL DEFAULT '',
			protocol INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'never',
			last_seen_at INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE node_containers (
			node_id INTEGER NOT NULL,
			container_id TEXT NOT NULL,
			container_name TEXT NOT NULL DEFAULT '',
			image_ref TEXT NOT NULL DEFAULT '',
			image_id TEXT NOT NULL DEFAULT '',
			local_tag TEXT NOT NULL DEFAULT '',
			local_digest TEXT NOT NULL DEFAULT '',
			remote_digest TEXT NOT NULL DEFAULT '',
			latest_tag TEXT NOT NULL DEFAULT '',
			aliases TEXT NOT NULL DEFAULT '',
			origin TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			mode TEXT NOT NULL DEFAULT '',
			ignored INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			checked_at INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO nodes (id, name, token_hash, status) VALUES (1, 'vps', 'h', 'online')`,
		`INSERT INTO node_containers (node_id, container_id, container_name, image_ref)
		 VALUES (1, 'abc', 'web', 'nginx:latest')`,
	}
	for _, stmt := range oldSchema {
		if _, err := raw.Exec(stmt); err != nil {
			raw.Close()
			t.Fatalf("造老库失败: %v", err)
		}
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("老库升级失败: %v", err)
	}
	defer s.Close()

	cols, err := s.tableColumns("node_containers")
	if err != nil {
		t.Fatal(err)
	}
	if !cols["compose_project"] {
		t.Fatal("node_containers 上没有 compose_project，老库升级后节点页会读不出来")
	}
	if ncols, _ := s.tableColumns("nodes"); ncols["compose_project"] {
		t.Error("compose_project 不该加在 nodes 上")
	}

	list, err := s.ListNodeContainers(1)
	if err != nil {
		t.Fatalf("读快照失败: %v", err)
	}
	if len(list) != 1 || list[0].ContainerName != "web" {
		t.Fatalf("老数据没读回来: %+v", list)
	}
	if list[0].ComposeProject != "" {
		t.Errorf("老数据的项目名应为空，实际 %q", list[0].ComposeProject)
	}

	// 补上列之后要能正常写入新上报的项目名，否则合并提醒永远拿不到依据。
	if err := s.ReplaceNodeContainers(1, []model.NodeContainer{
		{NodeID: 1, ContainerID: "abc", ContainerName: "web", ImageRef: "nginx:latest", ComposeProject: "webstack"},
	}); err != nil {
		t.Fatalf("写入带项目名的快照失败: %v", err)
	}
	got, err := s.FindNodeContainerByName(1, "web")
	if err != nil {
		t.Fatal(err)
	}
	if got.ComposeProject != "webstack" {
		t.Errorf("项目名没落盘，实际 %q", got.ComposeProject)
	}
}

// 1.11.0 给 images 与 node_containers 各加了两列（compose 服务名与依赖）。
//
// 与 1.10.0 那次事故同一个坑：CREATE TABLE IF NOT EXISTS 对已存在的表不补列，
// 补列清单写错表名则老库一升级就 no such column。这里用「没有这两列的老库」
// 起一次，断言两列都补在各自表上、并且能正常存取。
func TestMigrateAddsComposeDepsToBothTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	oldSchema := []string{
		`CREATE TABLE nodes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL DEFAULT '',
			token_hash TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'never',
			created_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE images (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			reference TEXT NOT NULL,
			container_id TEXT NOT NULL DEFAULT '',
			container_name TEXT NOT NULL DEFAULT '',
			compose_project TEXT NOT NULL DEFAULT '',
			origin TEXT NOT NULL DEFAULT '',
			image_id TEXT NOT NULL DEFAULT '',
			size_bytes INTEGER NOT NULL DEFAULT 0,
			image_created INTEGER NOT NULL DEFAULT 0,
			dangling INTEGER NOT NULL DEFAULT 0,
			local_digest TEXT NOT NULL DEFAULT '',
			remote_digest TEXT NOT NULL DEFAULT '',
			local_tag TEXT NOT NULL DEFAULT '',
			latest_tag TEXT NOT NULL DEFAULT '',
			aliases TEXT NOT NULL DEFAULT '',
			mode TEXT NOT NULL DEFAULT 'auto',
			mode_override INTEGER NOT NULL DEFAULT 0,
			ignored INTEGER NOT NULL DEFAULT 0,
			ignored_digest TEXT NOT NULL DEFAULT '',
			notified_tag TEXT NOT NULL DEFAULT '',
			notified_digest TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'unknown',
			baseline_at INTEGER NOT NULL DEFAULT 0,
			last_checked_at INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0,
			UNIQUE(reference, container_id)
		)`,
		`CREATE TABLE node_containers (
			node_id INTEGER NOT NULL,
			container_id TEXT NOT NULL,
			container_name TEXT NOT NULL DEFAULT '',
			compose_project TEXT NOT NULL DEFAULT '',
			image_ref TEXT NOT NULL DEFAULT '',
			image_id TEXT NOT NULL DEFAULT '',
			local_tag TEXT NOT NULL DEFAULT '',
			local_digest TEXT NOT NULL DEFAULT '',
			remote_digest TEXT NOT NULL DEFAULT '',
			latest_tag TEXT NOT NULL DEFAULT '',
			aliases TEXT NOT NULL DEFAULT '',
			origin TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			mode TEXT NOT NULL DEFAULT '',
			ignored INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			checked_at INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO nodes (id, name, token_hash, status) VALUES (1, 'vps', 'h', 'online')`,
		`INSERT INTO images (reference, container_id, container_name, compose_project, origin)
		 VALUES ('ghcr.io/x/web:1.0', 'abc', 'web-1', 'webstack', 'container')`,
		`INSERT INTO node_containers (node_id, container_id, container_name, compose_project, image_ref)
		 VALUES (1, 'abc', 'web-1', 'webstack', 'ghcr.io/x/web:1.0')`,
	}
	for _, stmt := range oldSchema {
		if _, err := raw.Exec(stmt); err != nil {
			raw.Close()
			t.Fatalf("造老库失败: %v", err)
		}
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("老库升级失败: %v", err)
	}
	defer s.Close()

	for _, table := range []string{"images", "node_containers"} {
		cols, err := s.tableColumns(table)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"compose_service", "compose_deps"} {
			if !cols[want] {
				t.Fatalf("%s 上没有 %s，升级后查快照会直接报 no such column", table, want)
			}
		}
	}

	// 老数据读得回来，新加的列是空的（默认值）。
	list, err := s.ListNodeContainers(1)
	if err != nil {
		t.Fatalf("读快照失败: %v", err)
	}
	if len(list) != 1 || list[0].ComposeService != "" || len(list[0].ComposeDeps) != 0 {
		t.Fatalf("老数据应读回空依赖，实际 %+v", list)
	}
	imgs, err := s.ListImages(model.OriginContainer)
	if err != nil {
		t.Fatalf("读监控项失败: %v", err)
	}
	if len(imgs) != 1 || imgs[0].ComposeService != "" {
		t.Fatalf("老监控项应读回空服务名，实际 %+v", imgs)
	}

	// 新上报要能把服务名与依赖写进去、读出来 —— 排序全靠这两个字段。
	if err := s.ReplaceNodeContainers(1, []model.NodeContainer{{
		NodeID: 1, ContainerID: "abc", ContainerName: "web-1", ImageRef: "ghcr.io/x/web:1.0",
		ComposeProject: "webstack", ComposeService: "web", ComposeDeps: []string{"db", "redis"},
	}}); err != nil {
		t.Fatalf("写入带依赖的快照失败: %v", err)
	}
	got, err := s.FindNodeContainerByName(1, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ComposeService != "web" {
		t.Errorf("服务名没落盘，实际 %q", got.ComposeService)
	}
	if len(got.ComposeDeps) != 2 || got.ComposeDeps[0] != "db" || got.ComposeDeps[1] != "redis" {
		t.Errorf("依赖列表没落盘，实际 %v", got.ComposeDeps)
	}

	if _, err := s.UpsertImage(&model.Image{
		Reference: "ghcr.io/x/web:1.0", ContainerID: "abc", ContainerName: "web-1",
		ComposeService: "web", ComposeDeps: []string{"db"},
	}); err != nil {
		t.Fatalf("写入监控项失败: %v", err)
	}
	imgs, err = s.ListImages(model.OriginContainer)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 || imgs[0].ComposeService != "web" || len(imgs[0].ComposeDeps) != 1 {
		t.Fatalf("监控项上的依赖没写对: %+v", imgs)
	}
}

// 1.12.0 给 nodes 加了 offline_alert_at（同一次失联只报一次靠它）。
//
// 又一次踩「CREATE TABLE IF NOT EXISTS 不补列」这条：老库升上来必须走
// ALTER TABLE，否则 Update 语句直接 no such column，整个失联告警全废。
func TestMigrateAddsOfflineAlertAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// 1.11 时代的 nodes 表：没有 offline_alert_at。
	for _, stmt := range []string{
		`CREATE TABLE nodes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL DEFAULT '',
			hub_url TEXT NOT NULL DEFAULT '',
			note TEXT NOT NULL DEFAULT '',
			token_hash TEXT NOT NULL DEFAULT '',
			hostname TEXT NOT NULL DEFAULT '',
			os TEXT NOT NULL DEFAULT '',
			arch TEXT NOT NULL DEFAULT '',
			agent_version TEXT NOT NULL DEFAULT '',
			protocol INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'never',
			last_seen_at INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO nodes (id, name, hub_url, token_hash, status) VALUES (1, 'waf', 'http://hub', 'h', 'online')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			raw.Close()
			t.Fatalf("造老库失败: %v", err)
		}
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("老库升级失败: %v", err)
	}
	defer s.Close()

	cols, err := s.tableColumns("nodes")
	if err != nil {
		t.Fatal(err)
	}
	if !cols["offline_alert_at"] {
		t.Fatal("nodes 上没有 offline_alert_at，升级后告警逻辑会直接报错")
	}

	// 老数据读得回，记号是 0（还没报过）。
	if at, err := s.NodeOfflineAlertAt(1); err != nil || at != 0 {
		t.Fatalf("老节点应当读回 0，实际 at=%d err=%v", at, err)
	}
	if err := s.SetNodeOfflineAlert(1, 1700000000); err != nil {
		t.Fatalf("写记号失败: %v", err)
	}
	if at, _ := s.NodeOfflineAlertAt(1); at != 1700000000 {
		t.Errorf("记号没落盘，实际 %d", at)
	}
	if err := s.ClearNodeOfflineAlert(1); err != nil {
		t.Fatalf("清记号失败: %v", err)
	}
	if at, _ := s.NodeOfflineAlertAt(1); at != 0 {
		t.Errorf("清完应当是 0，实际 %d", at)
	}

	// 启动重置只动状态，不能把记号一起抹了 —— 同一次失联不该再报一遍。
	if err := s.SetNodeOfflineAlert(1, 1700000000); err != nil {
		t.Fatal(err)
	}
	_ = s.TouchNode(1)
	if err := s.MarkAllNodesOffline(); err != nil {
		t.Fatalf("重置在线状态失败: %v", err)
	}
	if at, _ := s.NodeOfflineAlertAt(1); at != 1700000000 {
		t.Errorf("重启后记号要留着，实际 %d", at)
	}
}

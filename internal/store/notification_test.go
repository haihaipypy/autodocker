package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/haihaipypy/autodocker/internal/model"
)

// 两个渠道同时配好时，一条提醒会各发一份，两份消息 ID 互不相干。
// 投递列表必须能原样存回去 —— 少一条，那个渠道的卡片更新完也改不动。
func TestNotificationDeliveriesRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer s.Close()

	id, err := s.CreateNotification(&model.Notification{
		ImageRef: "demo/app:1.0",
		Kind:     model.NotifNewVersion,
		Title:    "t",
	})
	if err != nil {
		t.Fatalf("写通知失败: %v", err)
	}

	list := []model.Delivery{
		{Channel: model.ChannelFeishu, ChatRef: "oc_x", MessageID: "om_1"},
		{Channel: model.ChannelTelegram, ChatRef: "-100123", MessageID: "456"},
	}
	if err := s.SetNotificationDeliveries(id, list, model.CardPending); err != nil {
		t.Fatalf("写投递记录失败: %v", err)
	}

	got, err := s.GetNotification(id)
	if err != nil {
		t.Fatalf("读通知失败: %v", err)
	}
	if len(got.Deliveries) != 2 {
		t.Fatalf("应存下 2 条投递，得到 %d 条: %+v", len(got.Deliveries), got.Deliveries)
	}
	if got.Deliveries[1].Channel != model.ChannelTelegram || got.Deliveries[1].MessageID != "456" {
		t.Fatalf("Telegram 那条投递没存对: %+v", got.Deliveries[1])
	}
	// 主投递取第一条，给前端展示与老代码用
	if got.Channel != model.ChannelFeishu || got.MessageID != "om_1" {
		t.Fatalf("主投递应取列表第一条，得到 %q / %q", got.Channel, got.MessageID)
	}
}

// 纠正 Telegram 会话时只能改 Telegram 那一份，别把飞书的会话 ID 顺手覆盖掉。
func TestUpdateNotificationDeliveriesKeepsOthers(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer s.Close()

	id, _ := s.CreateNotification(&model.Notification{ImageRef: "demo/app:1.0", Kind: model.NotifNewVersion})
	list := []model.Delivery{
		{Channel: model.ChannelFeishu, ChatRef: "oc_x", MessageID: "om_1"},
		{Channel: model.ChannelTelegram, ChatRef: "-100123", MessageID: "456"},
	}
	if err := s.SetNotificationDeliveries(id, list, model.CardPending); err != nil {
		t.Fatalf("写投递记录失败: %v", err)
	}

	list[1].ChatRef = "-100999"
	if err := s.UpdateNotificationDeliveries(id, list); err != nil {
		t.Fatalf("改投递记录失败: %v", err)
	}

	got, _ := s.GetNotification(id)
	if got.Deliveries[1].ChatRef != "-100999" {
		t.Fatalf("Telegram 会话没改过来: %+v", got.Deliveries[1])
	}
	if got.Deliveries[0].ChatRef != "oc_x" {
		t.Fatalf("飞书那条被误改了: %+v", got.Deliveries[0])
	}
}

// 老库（1.2.4 之前）没有 deliveries 列的内容，读出来应为空而不是报错。
func TestNotificationWithoutDeliveriesIsEmpty(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer s.Close()

	id, _ := s.CreateNotification(&model.Notification{ImageRef: "demo/app:1.0", Kind: model.NotifNewVersion})
	if _, err := s.db.Exec(`UPDATE notifications SET deliveries = '' WHERE id = ?`, id); err != nil {
		t.Fatalf("构造老数据失败: %v", err)
	}

	got, err := s.GetNotification(id)
	if err != nil {
		t.Fatalf("读老记录失败: %v", err)
	}
	if len(got.Deliveries) != 0 {
		t.Fatalf("老记录不应有投递列表，得到 %+v", got.Deliveries)
	}
}

// 远端去重靠这张表：同一版说过就不再提，「稍后提醒」则必须放行。
//
// 这两个语义差一个字就会出问题 —— 把 snoozed 也算成「说过」，
// 卡片上那句「下次巡检会重新推送」就成了空话。
func TestHasNodeNotificationKey(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer s.Close()

	n := &model.Notification{
		ImageRef: "nginx:latest", ContainerID: "c1", ContainerName: "web",
		NodeID: 7, NodeName: "1panel", VersionKey: "digest:sha256:bbb",
		Kind: model.NotifNewVersion, Title: "t", CardState: model.CardPending,
	}
	id, err := s.CreateNotification(n)
	if err != nil {
		t.Fatalf("写通知失败: %v", err)
	}

	if ok, err := s.HasNodeNotificationKey(7, "c1", "digest:sha256:bbb"); err != nil || !ok {
		t.Errorf("已经提醒过的版本应被判定为重复（err=%v ok=%v）", err, ok)
	}
	// 别的节点、别的摘要、别的容器都不该被这条记录挡住
	for _, c := range []struct {
		nodeID      int64
		containerID string
		key         string
	}{
		{8, "c1", "digest:sha256:bbb"},
		{7, "c2", "digest:sha256:bbb"},
		{7, "c1", "digest:sha256:ccc"},
	} {
		if ok, _ := s.HasNodeNotificationKey(c.nodeID, c.containerID, c.key); ok {
			t.Errorf("不该被这条记录去重：%+v", c)
		}
	}

	// 「稍后提醒」要放行，否则下一轮永远不会再推
	if err := s.SetNotificationCardState(id, model.CardSnoozed); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HasNodeNotificationKey(7, "c1", "digest:sha256:bbb"); ok {
		t.Error("稍后提醒的版本不能被当成「已经说过」")
	}

	// 「忽略」是明确的「别再提了」，要挡住
	if err := s.SetNotificationCardState(id, model.CardIgnored); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HasNodeNotificationKey(7, "c1", "digest:sha256:bbb"); !ok {
		t.Error("用户已忽略的版本不该再提醒")
	}
}

// 节点字段要能原样读写：0 表示本机，老库的默认值也是 0。
func TestNotificationNodeFieldsRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer s.Close()

	id, err := s.CreateNotification(&model.Notification{
		ImageRef: "nginx:latest", ContainerID: "c1", ContainerName: "web",
		NodeID: 7, NodeName: "1panel", VersionKey: "tag:1.27",
		Kind: model.NotifNewTag, Title: "t", CardState: model.CardPending,
	})
	if err != nil {
		t.Fatalf("写通知失败: %v", err)
	}
	got, err := s.GetNotification(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeID != 7 || got.NodeName != "1panel" || got.VersionKey != "tag:1.27" {
		t.Errorf("节点字段没存住: %+v", got)
	}

	local, _ := s.CreateNotification(&model.Notification{ImageRef: "redis:7", Kind: model.NotifNewVersion, Title: "t"})
	l, _ := s.GetNotification(local)
	if l.NodeID != 0 || l.NodeName != "" {
		t.Errorf("本机通知不该带节点: %+v", l)
	}
}

// 任务要记下节点，历史列表才分得清这次重建发生在哪台机器。
func TestTaskNodeFieldsRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer s.Close()

	id, err := s.CreateTask(&model.Task{
		ImageRef: "nginx:1.24", ContainerID: "c1", ContainerName: "web",
		NodeID: 7, NodeName: "1panel", TriggeredBy: "hub",
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	list, err := s.ListTasks(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("任务列表不对: %+v", list)
	}
	if list[0].NodeID != 7 || list[0].NodeName != "1panel" {
		t.Errorf("任务没存住节点: %+v", list[0])
	}
}

// 单条删除只该动那一条，别把相邻的记录一起带走；
// 删不存在的要能明确报「没有」，而不是假装成功。
func TestDeleteNotification(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "del.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	a, err := st.CreateNotification(&model.Notification{ImageRef: "a:1", Kind: model.NotifNewVersion})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateNotification(&model.Notification{ImageRef: "b:1", Kind: model.NotifNewVersion})
	if err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteNotification(a); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := st.GetNotification(a); !errors.Is(err, ErrNotFound) {
		t.Errorf("删掉的那条应该查不到，实际 err=%v", err)
	}
	if _, err := st.GetNotification(b); err != nil {
		t.Errorf("相邻的一条不该被删: %v", err)
	}

	if err := st.DeleteNotification(a); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复删除应报 ErrNotFound，实际 %v", err)
	}
}

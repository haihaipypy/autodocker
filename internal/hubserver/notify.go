package hubserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/version"
)

// nodeVersionKey 一条远端更新项的去重键。
//
// 有更高的版本 tag 时用它：tag 变了就是新版本，用户的「忽略」只对那一版生效。
// 浮动 tag（内容变了、版本号没动）只能退回摘要 —— 那正是唯一能区分
// 「还是那一版」和「作者又推了一版」的东西。
//
// 两种 key 加不同前缀，避免 tag 恰好等于某个摘要短码时串味。
func nodeVersionKey(c model.NodeContainer) string {
	if c.LatestTag != "" {
		return "tag:" + c.LatestTag
	}
	if c.RemoteDigest != "" {
		return "digest:" + c.RemoteDigest
	}
	return ""
}

// nodeHit 快照里「这一轮要提醒」的一项。
type nodeHit struct {
	c    model.NodeContainer
	kind model.NotificationKind
	key  string
}

// notifyUpdates 把这一轮快照里「还没提醒过」的更新项推成提醒。
//
// 只在远端做：本机的通知由 scanner 在巡检过程中发，那条路上还有「忽略本版本」
// 记在 images 表上的去重逻辑，重复实现一遍只会让两边慢慢跑偏。
//
// 远端没有 images 那样的长驻状态（快照每轮整体替换），去重只能回头看通知记录，
// 判断依据是每个容器各自的 version_key。
//
// 同一 compose 项目里的服务通常一起发版，按项目合并成一条提醒：一个 6 服务的
// 项目一次推 6 条消息、点 6 次，手机上一屏全是它。项目名来自容器标签，
// 老 Agent 不上报这个字段，那时就退化成一条一个。
func (s *Server) notifyUpdates(nodeID int64, nodeName string, list []model.NodeContainer) {
	if s.cfg.Notifier == nil || nodeID <= 0 {
		return
	}
	var hits []nodeHit
	for _, c := range list {
		// 只提醒容器维度：闲置镜像与纯远端盯梢项在节点页没有操作入口，
		// 推出来也只能看着，点不了。
		if c.Origin != "" && c.Origin != model.OriginContainer {
			continue
		}
		if c.Ignored || c.ContainerID == "" {
			continue
		}
		var kind model.NotificationKind
		switch c.Status {
		case model.StatusNewTag:
			kind = model.NotifNewTag
		case model.StatusUpdatePending:
			kind = model.NotifNewVersion
		default:
			continue
		}

		key := nodeVersionKey(c)
		if key == "" {
			continue
		}
		if ok, err := s.st.HasNodeNotificationKey(nodeID, c.ContainerID, key); err != nil {
			s.log.Warn("查询节点提醒去重失败", "node", nodeName, "err", err)
			continue
		} else if ok {
			continue
		}
		hits = append(hits, nodeHit{c: c, kind: kind, key: key})
	}

	var pending []model.Notification
	for _, group := range groupNodeHits(hits) {
		n, err := s.createNodeNotification(nodeID, nodeName, group)
		if err != nil {
			s.log.Warn("写入节点提醒失败", "node", nodeName,
				"project", group[0].c.ComposeProject, "ref", group[0].c.ImageRef, "err", err)
			continue
		}
		s.log.Info("发现节点更新", "node", nodeName, "project", n.ComposeProject,
			"containers", n.MemberCount, "ref", n.ImageRef)
		pending = append(pending, *n)
	}

	// 推送单独放一个协程：渠道慢（企业微信长连接、钉钉建卡）时单条能等十几秒，
	// 几台机器一起报就是好几分钟。这段时间里 WS 读循环必须继续读 ——
	// 卡住它等于把这台机器后面的所有上报和回执都堵在门口。
	if len(pending) > 0 {
		go s.pushNodeNotifications(pending)
	}
}

// groupNodeHits 按 compose 项目分组，没有项目标签的容器各自一组。
func groupNodeHits(hits []nodeHit) [][]nodeHit {
	idx := map[string]int{}
	var out [][]nodeHit
	for _, h := range hits {
		key := h.c.ComposeProject
		if key == "" {
			key = "#" + h.c.ContainerID
		}
		if i, ok := idx[key]; ok {
			out[i] = append(out[i], h)
			continue
		}
		idx[key] = len(out)
		out = append(out, []nodeHit{h})
	}
	return out
}

// createNodeNotification 把一组更新项写成一条（可能合并的）提醒。
func (s *Server) createNodeNotification(nodeID int64, nodeName string, group []nodeHit) (*model.Notification, error) {
	first := group[0].c
	n := &model.Notification{
		ImageRef:       first.ImageRef,
		ContainerID:    first.ContainerID,
		ContainerName:  first.ContainerName,
		NodeID:         nodeID,
		NodeName:       nodeName,
		VersionKey:     group[0].key,
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
			ContainerID:   h.c.ContainerID,
			ContainerName: h.c.ContainerName,
			ImageRef:      h.c.ImageRef,
			VersionKey:    h.key,
			Kind:          h.kind,
			LocalTag:      h.c.LocalTag,
			LatestTag:     h.c.LatestTag,
			LocalSummary:  version.ShortDigest(h.c.LocalDigest),
			RemoteSummary: version.ShortDigest(h.c.RemoteDigest),
		})
	}
	n.Title, n.Body = buildNodeGroupText(nodeName, group)
	if _, err := s.st.CreateNotificationWithMembers(n, members); err != nil {
		return nil, err
	}
	return n, nil
}

// buildNodeGroupText 合并提醒的标题与正文。
func buildNodeGroupText(nodeName string, group []nodeHit) (title, body string) {
	if len(group) == 1 {
		return buildNodeText(nodeName, group[0].c, group[0].kind)
	}
	where := nodeName
	if where == "" {
		where = "远程节点"
	}
	project := group[0].c.ComposeProject
	if project == "" {
		project = "同一批容器"
	}
	title = fmt.Sprintf("🖥️ %s · %s（%d 个容器）有新版本可用", where, project, len(group))

	var b strings.Builder
	fmt.Fprintf(&b, "compose 项目 **%s** 在 **%s** 上有 %d 个容器可更新：", project, where, len(group))
	limit := len(group)
	if limit > 12 {
		limit = 12
	}
	for _, h := range group[:limit] {
		name := h.c.ContainerName
		if name == "" {
			name = h.c.ImageRef
		}
		if h.kind == model.NotifNewTag && h.c.LatestTag != "" {
			fmt.Fprintf(&b, "\n• %s  %s → **%s**", name, h.c.LocalTag, h.c.LatestTag)
		} else {
			fmt.Fprintf(&b, "\n• %s  %s（内容已更新）", name, h.c.LocalTag)
		}
	}
	if len(group) > limit {
		fmt.Fprintf(&b, "\n…另有 %d 个", len(group)-limit)
	}
	b.WriteString("\n\n点「立即更新」会在这台机器上依次重建这些容器，一次一个；某个失败会停下并说明原因。")
	return title, b.String()
}

// pushNodeNotifications 把已落库的节点提醒推到各个渠道。
func (s *Server) pushNodeNotifications(list []model.Notification) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for i := range list {
		n := list[i]
		if _, err := s.cfg.Notifier.Notify(ctx, &n); err != nil {
			// 推送失败不改任何状态：记录已经落库，页面里看得见，
			// 用户可以手动去点，下一轮也不会重复建一条。
			s.log.Warn("推送节点提醒失败", "node", n.NodeName, "ref", n.ImageRef, "err", err)
			continue
		}
		if len(n.Deliveries) > 0 {
			if err := s.st.SetNotificationDeliveries(n.ID, n.Deliveries, model.CardPending); err != nil {
				s.log.Warn("记录提醒投递坐标失败", "id", n.ID, "err", err)
			}
		}
		s.log.Info("已推送节点更新提醒", "node", n.NodeName, "container", n.ContainerName)
	}
}

// buildNodeText 远端提醒的标题与正文。
//
// 标题里必须带节点名：手机上一次收好几条提醒，只写容器名的话
// 「web 有新版本」根本分不清是哪台机器上的 web。
func buildNodeText(nodeName string, c model.NodeContainer, kind model.NotificationKind) (title, body string) {
	name := c.ContainerName
	if name == "" {
		name = c.ImageRef
	}
	where := nodeName
	if where == "" {
		where = "远程节点"
	}
	// 前面那个显示器符号是刻意加的：手机上连收几条提醒时，光看「web 有新版本」
	// 分不出是本机的还是某台远机器的 —— 本机提醒没有这个符号，一眼就能分开。
	title = fmt.Sprintf("🖥️ %s · %s 有新版本可用", where, name)

	switch kind {
	case model.NotifNewTag:
		body = fmt.Sprintf("当前版本 `%s`，仓库中已存在更高版本 **%s**。", c.LocalTag, c.LatestTag)
		if len(c.Aliases) > 0 {
			body += fmt.Sprintf("（同版本别名：%s）", strings.Join(c.Aliases, "、"))
		}
	default:
		body = fmt.Sprintf("镜像 `%s` 的内容已更新（tag 未变），本地仍是 %s。",
			c.ImageRef, version.ShortDigest(c.LocalDigest))
	}

	body += fmt.Sprintf("\n本次更新会在 **%s** 上重建容器，原挂载、端口与网络配置保持不变。", where)
	body += "\n点下方按钮直接在那台机器上执行，无需登录目标机。"
	return title, body
}

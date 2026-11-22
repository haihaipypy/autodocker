package notify

import (
	"strconv"

	"github.com/haihaipypy/autodocker/internal/composeorder"
	"github.com/haihaipypy/autodocker/internal/model"
)

// composeEdge 是一个容器的依赖信息：服务名 + depends_on 的服务名列表。
type composeEdge struct {
	service string
	deps    []string
}

// composeEdges 取一台机器上「容器名 → 依赖信息」的映射。
//
// 本机从 images 表读，远端从 node_containers 读：两处都存了同样的两个标签，
// 只是落库位置不同。查失败返回空映射，调用方据此退回原顺序 —— 排序是优化，
// 不能因为一次读库失败就整批不更新。
func (s *Service) composeEdges(nodeID int64) map[string]composeEdge {
	out := make(map[string]composeEdge)
	if nodeID == 0 {
		imgs, err := s.st.ListImages(model.OriginContainer)
		if err != nil {
			s.log.Warn("读取本机容器依赖用于排序失败", "err", err)
			return out
		}
		for _, img := range imgs {
			if img.ContainerName == "" {
				continue
			}
			out[img.ContainerName] = composeEdge{service: img.ComposeService, deps: img.ComposeDeps}
		}
		return out
	}
	list, err := s.st.ListNodeContainers(nodeID)
	if err != nil {
		s.log.Warn("读取节点容器依赖用于排序失败", "node", nodeID, "err", err)
		return out
	}
	for _, c := range list {
		if c.ContainerName == "" {
			continue
		}
		out[c.ContainerName] = composeEdge{service: c.ComposeService, deps: c.ComposeDeps}
	}
	return out
}

// sortMembersByDeps 把一条提醒里的成员按 compose 的 depends_on 排序。
//
// 在 Hub 侧排而不是让 Agent 排：Agent 每次只收到一个容器，它不知道这一批
// 里还有谁，也就无从判断先后。顺序必须在「整批下发之前」定下来。
//
// 拿不到依赖信息（老 Agent 没上报这两个标签、或者读库失败）就原样返回：
// 排不出顺序时保持快照顺序，比按名字猜安全。
func (s *Service) sortMembersByDeps(n *model.Notification, members []model.NotificationMember) []model.NotificationMember {
	if len(members) < 2 {
		return members
	}
	edges := s.composeEdges(n.NodeID)
	if len(edges) == 0 {
		return members
	}
	nodes := make([]composeorder.Node, 0, len(members))
	for i, m := range members {
		e := edges[m.ContainerName]
		// 用下标当标识：成员里没有唯一的容器编号（镜像维度的条目它可以是空的），
		// 而下标在一份切片里一定唯一，回填时不会张冠李戴。
		nodes = append(nodes, composeorder.Node{
			Name:    strconv.Itoa(i),
			Service: e.service,
			Deps:    e.deps,
		})
	}
	sorted := composeorder.Sort(nodes)
	out := make([]model.NotificationMember, 0, len(members))
	for _, nd := range sorted {
		i, err := strconv.Atoi(nd.Name)
		if err != nil || i < 0 || i >= len(members) {
			continue
		}
		out = append(out, members[i])
	}
	if len(out) != len(members) {
		// 理论上不会发生；真发生了说明排序结果不可信，宁可退回原顺序。
		return members
	}
	return out
}

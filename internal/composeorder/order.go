// Package composeorder 按 compose 的 depends_on 关系，给一批要重建的容器排序。
//
// 为什么需要它：重建不等于启动。`docker compose up` 会按 depends_on 排顺序，
// 而我们走 Docker API 自己 stop → rename → create → start，顺序完全取快照里的
// 行序（约等于创建时间或名字序）。下游先重建就会撞上上游正在重启的那几秒 ——
// safeline-tengine 起来后连不上 safeline-mgt，502、退出、被健康检查判失败，
// 用户看到的是「更新完网页打不开」，很难联想到是顺序问题。
//
// 排序只解决「谁先起来」。depends_on 本身不保证上游已经就绪（mgt 启动后还要
// 几十秒才 ready），那层由重建时等待健康检查兜住，两者配合才完整。
package composeorder

// Node 是排序需要知道的全部信息。
//
// 只取这三样，不碰容器结构体：Hub 手上有快照、通知里只有成员名，
// 两边的数据形状不一样，但都能翻译成这三个字段。
type Node struct {
	// Name 容器名，只在日志与兜底匹配时用。
	Name string
	// Service compose 服务名，来自标签 com.docker.compose.service。
	// 老 Agent 不上报，为空 —— 那就算没有依赖关系，保持原顺序。
	Service string
	// Deps 这个服务 depends_on 的服务名，来自标签 com.docker.compose.depends_on。
	Deps []string
}

// Sort 返回按依赖关系排好的新切片，不会改动入参。
//
// 稳定：没有依赖关系的容器保持调用方给的相对顺序。这一条很重要 ——
// 用户对照的是页面上那一列，一次更新把没依赖的容器顺序打乱，只会让人怀疑
// 别的东西也被动了。
//
// 依赖到本批之外的容器（只更新了项目里的一部分）按「已满足」处理：
// 那个容器这次不重建，不存在它正在重启的窗口。
func Sort(in []Node) []Node {
	n := len(in)
	if n < 2 {
		return append([]Node(nil), in...)
	}

	// 服务名 → 下标。同名的取第一个；项目内 service 本来就不该重名。
	idx := make(map[string]int, n)
	for i, nd := range in {
		if nd.Service == "" {
			continue
		}
		if _, dup := idx[nd.Service]; !dup {
			idx[nd.Service] = i
		}
	}

	// indeg[i] = i 依赖的、且也在本批里的服务个数。
	// users[j] = 依赖 j 的那些下标（j 一落位，它们的入度就减一）。
	indeg := make([]int, n)
	users := make([][]int, n)
	for i, nd := range in {
		seen := make(map[int]bool, len(nd.Deps))
		for _, d := range nd.Deps {
			j, ok := idx[d]
			if !ok || j == i || seen[j] {
				continue
			}
			seen[j] = true
			indeg[i]++
			users[j] = append(users[j], i)
		}
	}

	out := make([]Node, 0, n)
	done := make([]bool, n)
	for len(out) < n {
		// 每轮从前往后找第一个入度为零的：这一句就是「稳定」的来源。
		pick := -1
		for i := 0; i < n; i++ {
			if !done[i] && indeg[i] == 0 {
				pick = i
				break
			}
		}
		if pick < 0 {
			// 剩下的成一个环（compose 里写错了才会有）。整批不能因此不动，
			// 也不能丢节点 —— 按原顺序接在后面，让重建照常进行。
			for i := 0; i < n; i++ {
				if !done[i] {
					done[i] = true
					out = append(out, in[i])
				}
			}
			break
		}
		done[pick] = true
		out = append(out, in[pick])
		for _, u := range users[pick] {
			if !done[u] {
				indeg[u]--
			}
		}
	}
	return out
}

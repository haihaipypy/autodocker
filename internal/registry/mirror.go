package registry

import (
	"strings"
)

// DefaultMirror 内置的 Docker Hub 加速地址，逗号分隔，按推荐顺序排列。
//
// 国内直连 Docker Hub 基本不通，用户没填、Docker 守护进程也没配加速器时，
// 若还坚持「什么都不用」，镜像列表会整列显示「未知」，新装用户只会一脸茫然。
// 这里给一个开箱即用的默认值 —— 1ms.run 实测最快（几百毫秒），1panel 作备用。
//
// 备用源要挑「还愿意给匿名流量」的：轩源（xuanyuan.me）后来对任何仓库都回
// 403「免费节点当前繁忙」，排在第二位只会让每轮检测白等一次超时，已换掉。
//
// 写成带协议与结尾斜杠的完整形式只是为了和用户在设置页里看到的样子一致，
// 真正使用时会被 ParseMirrors 归一化成裸主机名。
const DefaultMirror = "https://docker.1ms.run/,https://docker.1panel.live/"

// ParseMirrors 解析镜像加速主机列表。
//
// 输入可以是用户在设置页里随手粘贴的一串地址，因此分隔符放宽：
// 逗号、分号、空白、换行都算；https:// 前缀与结尾斜杠会被剥掉；
// 地址后面误带的路径也会被丢弃 —— 公共加速站的路径规则是固定的，
// 用户写什么都不会被采用，与其猜不如不猜。
//
// 返回值顺序即回退优先级，重复项自动去掉。
func ParseMirrors(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', ';', '\n', '\r', '\t', ' ':
			return true
		}
		return false
	})
	out := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		host := normalizeMirrorHost(f)
		if host == "" {
			continue
		}
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	return out
}

// normalizeMirrorHost 把各种写法的地址收敛成裸主机名。
func normalizeMirrorHost(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	if s == "" {
		return ""
	}
	return strings.ToLower(s)
}

// IsDockerHub 判断归一化后的注册表主机是否属于 Docker Hub。
//
// 只有 Docker Hub 会被加速主机接管：公共加速站（DaoCloud、1ms、1panel、各高校）
// 基本只缓存 Docker Hub 的镜像，把 ghcr.io / quay.io / 私有仓库的请求也改发过去，
// 换来的只会是一片 404 —— 这正是早先「配了加速就全都读不到」的根源。
func IsDockerHub(host string) bool {
	switch host {
	case DefaultRegistryAPI, DefaultRegistry, "index.docker.io", "registry.hub.docker.com":
		return true
	}
	return false
}

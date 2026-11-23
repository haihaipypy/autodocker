// Package registry 负责解析镜像引用并与远端注册表交互，
// 提供「当前 tag 的 manifest digest」与「仓库 tag 列表」两项查询。
package registry

import (
	"errors"
	"strings"
)

const (
	// DefaultRegistry 用户书写习惯上的 Docker Hub 域名。
	DefaultRegistry = "docker.io"
	// DefaultRegistryAPI Docker Hub 实际提供 v2 API 的域名。
	DefaultRegistryAPI = "registry-1.docker.io"
)

// Reference 一个已解析的镜像引用。
type Reference struct {
	Registry   string // 归一化后的注册表主机，如 registry-1.docker.io
	Repository string // 仓库路径，如 library/nginx
	Tag        string // 标签，缺省为 latest
	Original   string // 原始书写形式，用于展示与日志
}

// String 还原成用于展示、并作为监控项唯一键的引用形式。
//
// 对 Docker Hub 会省掉隐含的 library/ 前缀：用户写的是 nginx:latest，
// 列表里就该显示 nginx:latest，而不是突兀的 library/nginx:latest。
// 顺带的好处是「nginx」「library/nginx」「docker.io/nginx」会归一到同一条记录。
// 注意：请求注册表用的路径走的是 Repository 字段，不受这里影响。
func (r Reference) String() string {
	host := r.Registry
	if host == DefaultRegistryAPI {
		host = DefaultRegistry
	}
	if host == DefaultRegistry {
		return strings.TrimPrefix(r.Repository, "library/") + ":" + r.Tag
	}
	return host + "/" + r.Repository + ":" + r.Tag
}

// FullRepository 返回 registry/repository，用于拼接 API 路径。
func (r Reference) FullRepository() string {
	return r.Registry + "/" + r.Repository
}

// WithTag 返回把 tag 换成新值的引用。
//
// 用于「锁定版本」场景：容器当前跑着 nginx:1.25，仓库里出现了 1.27，
// 更新时必须真的去拉 1.27，而不是把 1.25 再拉一遍。
//
// 尽量沿用原本的书写形式（nginx:1.25 → nginx:1.27），
// 免得日志和飞书卡片上突然冒出个 library/nginx:1.27 让人犯嘀咕。
func (r Reference) WithTag(tag string) string {
	if tag == "" || tag == r.Tag {
		if r.Original != "" {
			return r.Original
		}
		return r.String()
	}
	if base := r.baseForRef(); base != "" {
		return base + ":" + tag
	}
	r.Tag = tag
	return r.String()
}

// baseForRef 取出「原本书写形式」里 tag 之前的那一段。
//
// 只有在能确定切点正确时才返回非空：原串末尾必须确实是当前 tag，或者原串压根没有 tag。
// 一旦切点与解析结果对不上，宁可用归一化形式，也不能切出一个错的仓库名——
// 那会导致去拉一个根本不存在的镜像。
func (r Reference) baseForRef() string {
	s := strings.TrimSpace(r.Original)
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[:i] // 摘掉 @sha256:... 摘要固定
	}
	if s == "" {
		return ""
	}
	colon := strings.LastIndex(s, ":")
	slash := strings.LastIndex(s, "/")
	// colon 落在 slash 之后才是 tag；否则那个冒号属于端口号（registry:5000/repo）
	if colon >= 0 && colon > slash {
		if s[colon+1:] != r.Tag {
			return ""
		}
		return s[:colon]
	}
	return s
}

// ParseReference 按 Docker 的规则解析镜像引用。
//
// 规则：
//   - 第一段含 "." 或 ":" 或等于 localhost 时视为注册表主机，否则是 Docker Hub；
//   - Docker Hub 下的单段仓库自动补 library/ 前缀；
//   - 末段冒号之后是 tag；缺省为 latest；
//   - 忽略 @sha256:... 形式的摘要固定（返回时只保留 tag）。
func ParseReference(raw string) (Reference, error) {
	ref := strings.TrimSpace(raw)
	if ref == "" {
		return Reference{}, errors.New("镜像引用为空")
	}
	// 剥离摘要固定部分
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if ref == "" {
		return Reference{}, errors.New("镜像引用仅包含摘要，缺少仓库名")
	}

	var host, rest string
	if i := strings.Index(ref, "/"); i >= 0 {
		first := ref[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			host = first
			rest = ref[i+1:]
		} else {
			host = DefaultRegistry
			rest = ref
		}
	} else {
		host = DefaultRegistry
		rest = ref
	}

	tag := "latest"
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		tag = rest[i+1:]
		rest = rest[:i]
	}
	repo := strings.Trim(rest, "/")
	if repo == "" {
		return Reference{}, errors.New("镜像引用缺少仓库名")
	}
	if tag == "" {
		tag = "latest"
	}
	if host == DefaultRegistry && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}

	return Reference{
		Registry:   normalizeHost(host),
		Repository: repo,
		Tag:        tag,
		Original:   strings.TrimSpace(raw),
	}, nil
}

// IsImageID 判断一个「镜像引用」其实是不是镜像 ID。
//
// 容器的 Config.Image 里不一定存着仓库引用：用 docker run <镜像ID> 起的容器、
// 或者在面板里按 ID 重建过，存的是一串十六进制 ID。这种值照样能被
// ParseReference 解析成 library/c4d252255b7b:latest —— 语法上合法、语义上胡说，
// 拿去查注册表必然「不存在」，而用户看到的是「所有镜像源均失败」，
// 跟镜像源一点关系都没有。
//
// 只认裸的纯十六进制串（12~64 位，可带 sha256: 前缀）：带斜杠或冒号的值
// 一律按引用处理 —— 把它误判成 ID 会让正常的镜像失去检测能力。
func IsImageID(s string) bool {
	s = strings.TrimPrefix(strings.TrimSpace(s), "sha256:")
	if len(s) < 12 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// normalizeHost 把用户写法归一化成实际 API 主机。
func normalizeHost(host string) string {
	switch host {
	case DefaultRegistry, "index.docker.io", "registry.hub.docker.com":
		return DefaultRegistryAPI
	}
	return host
}

// ParseRepoTags 从 "nginx:1.25" 或 "ghcr.io/a/b:v1" 中同时取出仓库与 tag，
// 便于把容器镜像引用拆成「仓库 + 当前 tag」两部分。
func ParseRepoTags(image string) (repo string, tag string) {
	r, err := ParseReference(image)
	if err != nil {
		return image, ""
	}
	return r.FullRepository(), r.Tag
}

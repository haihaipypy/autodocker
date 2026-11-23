// Package composetag 把 compose 项目里「固定版本的那一处」改到目标版本。
//
// 为什么需要它：compose 起的容器，镜像 tag 由 compose 文件（或它读的 .env）决定。
// 我们更新容器是「按容器配置原样重建」，容器换到了新 tag、文件里还是旧的 ——
// 之后任何人跑一次 docker compose up -d，都会把它们退回旧版本。safeline 那台
// 机器从 9.4.7 掉回 9.4.3 就是这么发生的：三个容器被单独更新过，.env 一直写着 9.4.3。
//
// 这里只做纯文本的「读懂 → 核对 → 改写」，不碰 Docker、不碰网络，
// 好让每一条判断都能被单测钉住。安全性靠三道核对：
//  1. 文件里解析出来的引用必须与容器当前跑的引用完全一致（对不上就一个字节都不改）；
//  2. 仓库名必须与目标一致（防止把 nginx 改成 redis）；
//  3. 拿不准的形态（digest 固定、变量解不出来、tag 由多个变量拼成）一律拒绝。
package composetag

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/haihaipypy/autodocker/internal/registry"
)

// Files 一个 compose 项目里我们能改的两个文件。
type Files struct {
	ComposePath string
	Compose     string
	// EnvPath 为空表示项目目录里没有 .env。
	EnvPath string
	Env     string
}

// Request 一次改写请求。
type Request struct {
	Service string // compose 服务名（容器标签 com.docker.compose.service）
	Current string // 容器现在跑的引用，如 chaitin/safeline-mgt:9.4.3
	Target  string // 这次要去的引用，如 chaitin/safeline-mgt:9.4.8
}

// Result 改写结果。Changed 为 false 时 Compose / Env 无意义。
type Result struct {
	Changed bool
	Compose string
	Env     string
	Note    string
}

// Plan 算出该怎么改。返回 error 表示「不能改」，调用方应当原样放过、只留提示。
func Plan(f Files, req Request) (Result, error) {
	if req.Service == "" {
		return Result{}, fmt.Errorf("容器上没有 compose 服务名标签")
	}
	if registry.IsImageID(req.Current) {
		return Result{}, fmt.Errorf("容器记录的是镜像 ID，不是仓库引用")
	}
	target, err := registry.ParseReference(req.Target)
	if err != nil {
		return Result{}, fmt.Errorf("目标引用 %s 解析不了：%w", req.Target, err)
	}
	currentKey, ok := refKey(req.Current)
	if !ok {
		return Result{}, fmt.Errorf("容器当前引用 %s 解析不了", req.Current)
	}

	lineNo, imageValue, err := findImageLine(f.Compose, req.Service)
	if err != nil {
		return Result{}, err
	}
	repo, tag, hasTag := splitImage(imageValue)
	if !hasTag {
		return Result{}, fmt.Errorf("服务 %s 的 image 没写 tag（%s），这种写法没有可改的地方", req.Service, imageValue)
	}
	if strings.Contains(imageValue, "@sha256:") {
		return Result{}, fmt.Errorf("服务 %s 的 image 用 digest 固定了（%s），改 tag 没有意义", req.Service, imageValue)
	}

	env := parseEnv(f.Env)
	// 先按严格规则展开；只有在「解不出来」时才退回宽松规则（未定义的变量当空串），
	// 因为 compose 自己的行为就是「未定义 → 空串」。宽松结果仍要通过下面的三重核对。
	resolvedRepo, ok := expand(repo, env)
	if !ok {
		resolvedRepo, _ = expandLenient(repo, env)
	}
	if resolvedRepo == "" || strings.ContainsAny(resolvedRepo, "${") {
		return Result{}, fmt.Errorf("服务 %s 的 image 里有解不出来的变量：%s", req.Service, imageValue)
	}
	if !sameRepo(resolvedRepo, target.Repository) {
		return Result{}, fmt.Errorf("compose 里写的是 %s，这次要更新的却是 %s，不是同一个仓库",
			resolvedRepo, target.Repository)
	}

	fileTag := tag
	if strings.Contains(tag, "$") {
		t, ok := expand(tag, env)
		if !ok {
			t, _ = expandLenient(tag, env)
		}
		if t == "" || strings.Contains(t, "$") {
			return Result{}, fmt.Errorf("服务 %s 的 tag 是变量 %s，但 .env 里没有它的值", req.Service, tag)
		}
		fileTag = t
	}

	oldKey := normalizeRepo(resolvedRepo) + ":" + fileTag
	targetKey := normalizeRepo(target.Repository) + ":" + target.Tag
	switch {
	case fileTag == target.Tag:
		return Result{Note: fmt.Sprintf("%s 里已经是 %s，不用改", filepath.Base(f.ComposePath), target.Tag)}, nil
	case oldKey != currentKey && currentKey != targetKey:
		// 文件与容器对不上，说明这个容器不是照这份文件建的（或者有人手改过）。
		// 此时去改文件等于赌一把，宁可不改。
		// 只有一种例外：容器已经在目标版本上 —— 那是「上次更新成功但文件没跟上」，
		// 补上正好，也是这个功能要修的老账。
		return Result{}, fmt.Errorf("compose 文件指向 %s，容器跑的是 %s，两者对不上，不动文件",
			resolvedRepo+":"+fileTag, req.Current)
	}

	// 变量形态：改 .env 里的那一行，compose 文件一个字节都不动。
	if name := soleVarName(tag); name != "" {
		if f.EnvPath == "" {
			return Result{}, fmt.Errorf("服务 %s 的 tag 用的是 ${%s}，但项目目录里没有 .env", req.Service, name)
		}
		newEnv, ok := setEnvValue(f.Env, name, target.Tag)
		if !ok {
			return Result{}, fmt.Errorf(".env 里找不到 %s= 这一行", name)
		}
		return Result{
			Changed: true,
			Compose: f.Compose,
			Env:     newEnv,
			Note: fmt.Sprintf("%s：把 %s 里的 %s 由 %s 改成 %s（原文件已备份为 %s.autodocker.bak）",
				req.Service, filepath.Base(f.EnvPath), name, fileTag, target.Tag, filepath.Base(f.EnvPath)),
		}, nil
	}

	// 字面量形态：只替换 image 那一行里的引用。
	out := lines(f.Compose)
	newLine := strings.Replace(out[lineNo], repo+":"+tag, repo+":"+target.Tag, 1)
	if newLine == out[lineNo] {
		return Result{}, fmt.Errorf("没能在 image 那一行里找到 %s，不动文件", repo+":"+tag)
	}
	out[lineNo] = newLine
	return Result{
		Changed: true,
		Compose: strings.Join(out, "\n"),
		Env:     f.Env,
		Note: fmt.Sprintf("%s：把 %s 里的 %s 改成 %s（原文件已备份为 %s.autodocker.bak）",
			req.Service, filepath.Base(f.ComposePath), repo+":"+tag, repo+":"+target.Tag, filepath.Base(f.ComposePath)),
	}, nil
}

// findImageLine 找出某个服务的 image 那一行的行号与值。
//
// 只认 services: 下与其它服务同级的条目，且 image 必须是该服务的直接子项 ——
// 宁可找不到，也不能把别的服务的镜像当成它的。
func findImageLine(text, service string) (int, string, error) {
	rows := lines(text)
	inServices := false
	servicesIndent, svcIndent := -1, -1
	start := -1
	for i, raw := range rows {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := indentOf(raw)
		if !inServices {
			if trimmed == "services:" {
				inServices = true
				servicesIndent = indent
			}
			continue
		}
		if indent <= servicesIndent {
			break // 出了 services 段
		}
		if svcIndent < 0 {
			svcIndent = indent
		}
		if indent == svcIndent && isServiceKey(trimmed, service) {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, "", fmt.Errorf("compose 文件里没有服务 %s", service)
	}

	for i := start + 1; i < len(rows); i++ {
		raw := rows[i]
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if indentOf(raw) <= svcIndent {
			break // 下一个服务了
		}
		if !strings.HasPrefix(trimmed, "image:") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(trimmed, "image:"))
		v = stripInlineComment(v)
		v = strings.Trim(v, `"'`)
		if v == "" {
			return 0, "", fmt.Errorf("服务 %s 的 image 是空的", service)
		}
		return i, v, nil
	}
	return 0, "", fmt.Errorf("服务 %s 里没有写 image", service)
}

// isServiceKey 判断这一行是不是名为 service 的键。
//
// YAML 允许 `web: # 注释` 这种写法，所以冒号后面只认空的或注释。
func isServiceKey(trimmed, service string) bool {
	idx := strings.IndexByte(trimmed, ':')
	if idx <= 0 {
		return false
	}
	if strings.Trim(trimmed[:idx], `"'`) != service {
		return false
	}
	rest := strings.TrimSpace(trimmed[idx+1:])
	return rest == "" || strings.HasPrefix(rest, "#")
}

// splitImage 把 chaitin/safeline-mgt:9.4.7 拆成仓库与 tag。
//
// 带端口的注册表（myreg:5000/app）不能把端口当成 tag，所以要找
// 「最后一个 / 之后的冒号」。
func splitImage(v string) (repo, tag string, hasTag bool) {
	// 注意 ${IMAGE_TAG:?msg} 里也有冒号：必须跳过花括号内部，
	// 否则会把 tag 切成 "${IMAGE_TAG" 与 "?msg}"。
	depth := 0
	lastSlash, lastColon := -1, -1
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '{':
			if i > 0 && v[i-1] == '$' {
				depth++
			}
		case '}':
			if depth > 0 {
				depth--
			}
		case '/':
			if depth == 0 {
				lastSlash = i
			}
		case ':':
			if depth == 0 {
				lastColon = i
			}
		}
	}
	if lastColon < 0 || lastColon < lastSlash {
		return v, "", false
	}
	return v[:lastColon], v[lastColon+1:], true
}

// soleVarName 处理 tag 整体就是一个变量的情况（${IMAGE_TAG}、${IMAGE_TAG:?msg}）。
func soleVarName(tag string) string {
	if !strings.HasPrefix(tag, "${") || !strings.HasSuffix(tag, "}") {
		return ""
	}
	expr := tag[2 : len(tag)-1]
	name, _, _ := splitVarExpr(expr)
	if name == "" || strings.ContainsAny(name, "{}$") {
		return "" // ${A}${B} 这种拼出来的 tag，不按单变量处理
	}
	return name
}

// splitVarExpr 拆 ${NAME} / ${NAME:-默认} / ${NAME-默认} / ${NAME:?提示} / ${NAME?提示}。
//
// 四种算符的语义并不一样：带冒号的（:- :?）把「定义了但为空」也算未设置，
// 不带的只看「有没有定义过」。safeline 的 .env 里 REGION= 正是空值，
// 把这两种混为一谈会把能用的文件判成解不出来。
func splitVarExpr(expr string) (name, op, arg string) {
	for i := 0; i < len(expr); i++ {
		rest := expr[i:]
		switch {
		case strings.HasPrefix(rest, ":-"):
			return expr[:i], ":-", rest[2:]
		case strings.HasPrefix(rest, ":?"):
			return expr[:i], ":?", rest[2:]
		case strings.HasPrefix(rest, "-"):
			return expr[:i], "-", rest[1:]
		case strings.HasPrefix(rest, "?"):
			return expr[:i], "?", rest[1:]
		}
	}
	return expr, "", ""
}

// expand 按 compose 的插值规则展开变量，遇到没定义的就报失败。
func expand(s string, env map[string]string) (string, bool) {
	out, ok := interpolate(s, env, false)
	return out, ok
}

// expandLenient 把未定义的变量当空串 —— compose 自己就是这么做的。
func expandLenient(s string, env map[string]string) (string, bool) {
	return interpolate(s, env, true)
}

func interpolate(s string, env map[string]string, lenient bool) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return "", false
			}
			name, op, arg := splitVarExpr(s[i+2 : i+end])
			v, ok := env[name]
			switch op {
			case ":-":
				if !ok || v == "" {
					v = arg
				}
			case "-":
				if !ok {
					v = arg
				}
			case ":?", "?":
				if !ok || (op == ":?" && v == "") {
					if !lenient {
						return "", false
					}
					v = ""
				}
			default:
				if !ok {
					if !lenient {
						return "", false
					}
					v = ""
				}
			}
			b.WriteString(v)
			i += end + 1
			continue
		}
		j := i + 1
		for j < len(s) && (isIdent(s[j])) {
			j++
		}
		if j == i+1 {
			b.WriteByte('$')
			i++
			continue
		}
		name := s[i+1 : j]
		v, ok := env[name]
		if !ok {
			if !lenient {
				return "", false
			}
			v = ""
		}
		b.WriteString(v)
		i = j
	}
	return b.String(), true
}

func isIdent(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// parseEnv 读 .env。只认 KEY=VALUE，够 compose 插值用。
func parseEnv(text string) map[string]string {
	out := map[string]string{}
	for _, raw := range lines(text) {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		out[key] = unquote(strings.TrimSpace(line[eq+1:]))
	}
	return out
}

// setEnvValue 改 .env 里的一个值，引号风格保持不变。
func setEnvValue(text, name, value string) (string, bool) {
	out := lines(text)
	for i, raw := range out {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") {
			continue
		}
		body := strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(body, '=')
		if eq <= 0 || strings.TrimSpace(body[:eq]) != name {
			continue
		}
		head := raw[:strings.Index(raw, "=")+1]
		old := strings.TrimSpace(raw[strings.Index(raw, "=")+1:])
		quote := ""
		if len(old) >= 2 && (old[0] == '"' || old[0] == '\'') && old[len(old)-1] == old[0] {
			quote = string(old[0])
		}
		out[i] = head + quote + value + quote
		return strings.Join(out, "\n"), true
	}
	return "", false
}

// refKey 把引用归一成「仓库:tag」，用于比较文件与容器的写法差异。
func refKey(s string) (string, bool) {
	r, err := registry.ParseReference(s)
	if err != nil {
		return "", false
	}
	tag := r.Tag
	if tag == "" {
		tag = "latest"
	}
	return normalizeRepo(r.Repository) + ":" + tag, true
}

func sameRepo(a, b string) bool {
	return normalizeRepo(a) == normalizeRepo(b)
}

func normalizeRepo(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimPrefix(s, "docker.io/")
	s = strings.TrimPrefix(s, "index.docker.io/")
	s = strings.TrimPrefix(s, "registry-1.docker.io/")
	s = strings.TrimPrefix(s, "library/")
	return s
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// stripInlineComment 去掉行尾注释：YAML 里只有「空格 + #」才算注释，
// 所以 myrepo/app#1 这种名字不会被误伤。
func stripInlineComment(s string) string {
	if i := strings.Index(s, " #"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	if i := strings.Index(s, "\t#"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func indentOf(s string) int {
	return len(s) - len(strings.TrimLeft(s, " \t"))
}

// lines 切行并去掉 \r：compose 文件在 Windows 上编辑过也不该让匹配全盘失效。
func lines(s string) []string {
	out := strings.Split(s, "\n")
	for i := range out {
		out[i] = strings.TrimSuffix(out[i], "\r")
	}
	return out
}

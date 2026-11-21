package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/haihaipypy/autodocker/internal/hubproto"
	"github.com/haihaipypy/autodocker/internal/model"
	"github.com/haihaipypy/autodocker/internal/selfcheck"
)

// 节点令牌直接沿用推送令牌的规格（16 字节随机数 = 32 个十六进制字符，
// 约 128 位熵）。再长对暴力破解没有实质帮助，反而让命令更难抄。
func newNodeToken() (string, error) { return newPushToken() }

func hashNodeToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// handleListNodes 节点列表。
func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.st.ListNodes()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取节点失败: "+err.Error())
		return
	}
	if nodes == nil {
		nodes = []model.Node{}
	}
	writeOK(w, map[string]any{"nodes": nodes, "online": s.hub.OnlineCount()})
}

type createNodeReq struct {
	Name   string `json:"name"`
	HubURL string `json:"hub_url"`
	Note   string `json:"note"`
}

// handleCreateNode 新建节点，返回令牌明文与可复制的安装命令。
//
// 令牌明文只在这里出现一次：库里存的是 sha256，之后无论怎么查都拿不回来。
// 想再要一条安装命令，只能重新生成令牌（旧的那条立刻失效）。
func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	var req createNodeReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "参数解析失败: "+err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.HubURL = strings.TrimRight(strings.TrimSpace(req.HubURL), "/")
	req.Note = strings.TrimSpace(req.Note)

	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "请填写节点名称")
		return
	}
	if msg := validateHubURL(req.HubURL); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}

	token, err := newNodeToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成令牌失败: "+err.Error())
		return
	}
	id, err := s.st.CreateNode(&model.Node{
		Name:   req.Name,
		HubURL: req.HubURL,
		Note:   req.Note,
	}, hashNodeToken(token))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeOK(w, map[string]any{
		"id":         id,
		"token":      token,
		"command":    s.installCommand(req.HubURL, token),
		"install_sh": s.installScriptURL(req.HubURL, id),
	})
}

type updateNodeReq struct {
	Name string `json:"name"`
	Note string `json:"note"`
}

// handleUpdateNode 改名与备注。
func (s *Server) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var req updateNodeReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "参数解析失败: "+err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "节点名称不能为空")
		return
	}
	if err := s.st.UpdateNodeMeta(id, req.Name, strings.TrimSpace(req.Note)); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"ok": true})
}

// handleDeleteNode 删节点。只删 Hub 侧的记录与快照，
// 目标机上的 Agent 容器不会自己消失 —— 需要用户在那边手动删。
func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.DeleteNode(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"ok": true})
}

// handleRotateNodeToken 换令牌。旧令牌立刻失效，已装的 Agent 会掉线，
// 必须在目标机上用新命令重装一次。
func (s *Server) handleRotateNodeToken(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	node, err := s.st.GetNode(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	token, err := newNodeToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成令牌失败: "+err.Error())
		return
	}
	if err := s.st.RotateNodeToken(id, hashNodeToken(token)); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{
		"token":      token,
		"command":    s.installCommand(node.HubURL, token),
		"install_sh": s.installScriptURL(node.HubURL, id),
	})
}

// handleNodeScan 让某个节点立刻巡检一轮。
func (s *Server) handleNodeScan(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	force := r.URL.Query().Get("force") == "1"
	if err := s.hub.Send(id, hubproto.ActionScan, hubproto.Cmd{Force: force}); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeOK(w, map[string]any{"ok": true})
}

// handleNodeContainers 某个节点的容器快照。
func (s *Server) handleNodeContainers(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	list, err := s.st.ListNodeContainers(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取快照失败: "+err.Error())
		return
	}
	if list == nil {
		list = []model.NodeContainer{}
	}
	writeOK(w, map[string]any{"containers": list})
}

// handleAllNodeContainers 所有节点的容器，供「全部节点」视图用。
func (s *Server) handleAllNodeContainers(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListAllNodeContainers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取快照失败: "+err.Error())
		return
	}
	if list == nil {
		list = []model.NodeContainerView{}
	}
	writeOK(w, map[string]any{"containers": list})
}

// handleInstallScript 输出安装脚本。
//
// 脚本内容只依赖 Hub 地址与镜像名，不含令牌 —— 令牌由命令行参数传入
// （见 installCommand）。所以它可以公开读取：泄露了也只能看到一段装容器的脚本。
//
// 地址优先取节点记录里那一份（用户生成命令时填的），而不是这次请求的 Host：
// 用户可能从内网地址打开页面、却打算把 Agent 装到外网机器上，
// 这时候用 r.Host 会把内网地址写进脚本，目标机永远连不上。
func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	hubURL := s.publicHubURL(r)
	if raw := r.URL.Query().Get("node"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
			if n, gerr := s.st.GetNode(id); gerr == nil && strings.TrimSpace(n.HubURL) != "" {
				hubURL = n.HubURL
			}
		}
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, scriptTemplate(hubURL, s.agentImage()))
}

// publicHubURL 推导 Hub 对外的地址。
//
// 优先用请求头里的 Host：用户从哪个地址打开 Hub，脚本里就写哪个地址 ——
// 这也是「填写 Hub 地址」那一格的默认值来源。反代后面要认 X-Forwarded-Proto，
// 否则 https 站点会生成 http 地址，Agent 连上去会被重定向或直接失败。
func (s *Server) publicHubURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); p != "" {
		scheme = strings.TrimSpace(strings.Split(p, ",")[0])
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	if host == "" {
		host = fmt.Sprintf("127.0.0.1:%d", s.cfg.Port)
	}
	return scheme + "://" + host
}

// agentImage 镜像方式的回退镜像。
//
// 主路径已经不靠镜像了（见 handleAgentBinary）：Hub 直接下发自己的可执行文件。
// 只有目标机架构与 Hub 不同时才会走到这里 —— 跨架构没法下发二进制，
// 只能让目标机去拉一个对应架构的镜像。
func (s *Server) agentImage() string {
	if v := strings.TrimSpace(s.cfg.AgentImage); v != "" {
		return v
	}
	// 跟上 Hub 自己的版本，而不是笼统的 latest：latest 指向什么永远是未知数，
	// 「装了却跑成旧版」正是这种不确定性造成的。
	tag := strings.TrimSpace(selfcheck.Version)
	if tag == "" || tag == "dev" {
		tag = "latest"
	}
	return "haihaipypy/autodocker:" + tag
}

// nodeTokenFrom 取节点令牌。
//
// 只认请求头，不认 query：URL 会进访问日志，令牌落在日志里等于泄露。
func nodeTokenFrom(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Agent-Token")); v != "" {
		return v
	}
	if b := strings.TrimSpace(r.Header.Get("Authorization")); len(b) > 7 && strings.EqualFold(b[:7], "bearer ") {
		return strings.TrimSpace(b[7:])
	}
	return ""
}

// 抽成变量而不是直接读 runtime：测试要在非 Linux 的机器上验证下发流程，
// 直接读的话这几个分支在 Windows/macOS 上永远走不到。
var (
	hostGOOS   = runtime.GOOS
	hostGOARCH = runtime.GOARCH
)

// handleAgentBinary 把 Hub 自己这个可执行文件发给 Agent。
//
// 这是「版本永远一致」的做法：Agent 跑的不是某个镜像标签里的代码，
// 而是和 Hub 同一份字节。Hub 是 1.7.3，装上去的就是 1.7.3，
// 不存在「latest 指向旧版本、装完不工作」这种情况，目标机也不需要访问 Docker Hub。
//
// 只发本机架构那一份：镜像里没有交叉编译产物，Go 也不能在运行时造一个出来。
// 跨架构（x86 的 Hub 管 ARM 的 NAS）由安装脚本回退到镜像方式。
func (s *Server) handleAgentBinary(w http.ResponseWriter, r *http.Request) {
	// 二进制本身不是机密（镜像里也有），但接口敞开会被当成免费 CDN。
	// 令牌先于平台判断：没令牌的人不必知道这台 Hub 跑在什么系统上。
	if _, err := s.st.FindNodeByTokenHash(hashNodeToken(nodeTokenFrom(r))); err != nil {
		writeErr(w, http.StatusUnauthorized, "节点令牌无效")
		return
	}
	if hostGOOS != "linux" {
		writeErr(w, http.StatusBadRequest, "当前 Hub 不是 Linux 版本，没有可供下发的 Agent 二进制。请用镜像方式安装。")
		return
	}
	want := strings.TrimSpace(r.URL.Query().Get("arch"))
	if want == "" {
		want = hostGOARCH
	}
	if want != hostGOARCH {
		writeErr(w, http.StatusConflict, fmt.Sprintf(
			"本 Hub 是 %s 架构，下发不了 %s 的二进制。请改用镜像方式：在安装命令后面加 --image",
			hostGOARCH, want))
		return
	}

	exe, err := os.Executable()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "找不到自身可执行文件: "+err.Error())
		return
	}
	// fpk 里那个路径可能是软链，解一下再发，免得发出去的是个链接本身。
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	f, err := os.Open(exe)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取自身可执行文件失败: "+err.Error())
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取文件信息失败: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="autodocker"`)
	w.Header().Set("Cache-Control", "no-store")
	// ServeContent 支持 Range 与 If-Modified-Since：
	// 目标机网络不好时 curl -C - 能续传，不用整包重来。
	http.ServeContent(w, r, "autodocker", st.ModTime(), f)
}

// handleInstallScript 输出安装脚本。

// validateHubURL 校验用户填的地址。返回空串表示通过。
//
// 只校验形态，不校验连通性：在 Hub 本机去测一个公网地址没有意义，
// 局域网地址更是可能只有目标机能到。能不能通，填的人心里得有数。
func validateHubURL(raw string) string {
	if raw == "" {
		return "请填写 Hub 访问地址（内网 IP 或公网域名）"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "Hub 访问地址格式不正确"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "Hub 访问地址必须以 http:// 或 https:// 开头"
	}
	if u.Host == "" {
		return "Hub 访问地址缺少主机名"
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "Hub 访问地址不要带查询参数或锚点"
	}
	return ""
}

func (s *Server) installCommand(hubURL, token string) string {
	return fmt.Sprintf("curl -fsSL %s/api/agent/install.sh | sh -s -- --token=%s",
		strings.TrimRight(hubURL, "/"), token)
}

func (s *Server) installScriptURL(hubURL string, nodeID int64) string {
	return fmt.Sprintf("%s/api/agent/install.sh?node=%d", strings.TrimRight(hubURL, "/"), nodeID)
}

// scriptTemplate 生成安装脚本。
//
// 主路径不拉镜像：Hub 把自己的可执行文件发下来，挂进一个 alpine 容器里跑。
// 好处是版本永远和 Hub 一致，也不要求目标机能访问 Docker Hub（国内 NAS 常拉不动）。
// 跨架构时（x86 的 Hub 管 ARM 的 NAS）才回退到镜像，加 --image 即可。
//
// 保持 POSIX sh（不依赖 bash），群晖、飞牛、OpenWrt 都能跑；
// 不做任何「顺手改系统配置」的事 —— 不写 systemd unit、不碰 iptables，
// 只拉二进制、换容器。幂等：重复执行就是升级，数据卷保留。
func scriptTemplate(hubURL, image string) string {
	return fmt.Sprintf(`#!/bin/sh
# AutoDocker Agent 安装脚本
# 由 Hub 自动生成：Hub 地址与回退镜像名都已写死，节点令牌通过参数传入。
set -eu

HUB_URL=%s
IMAGE=%s
CONTAINER=autodocker-agent
VOLUME=autodocker-agent-data
BINDIR=/var/lib/autodocker-agent
BASE_IMAGE=alpine:3.24
MODE=binary

TOKEN=""
for arg in "$@"; do
  case "$arg" in
    --token=*) TOKEN="${arg#--token=}" ;;
    --image)   MODE=image ;;
  esac
done

if [ -z "$TOKEN" ]; then
  echo "缺少节点令牌。" >&2
  echo "用法: curl -fsSL %s/api/agent/install.sh | sh -s -- --token=<节点令牌>" >&2
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "未找到 docker 命令，请先安装 Docker。" >&2
  exit 1
fi

if ! docker info >/dev/null 2>&1; then
  echo "Docker 守护进程没有响应，或当前用户没有访问权限。" >&2
  echo "请确认 Docker 正在运行，必要时用 sudo 重新执行。" >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *)             ARCH=$(uname -m) ;;
esac

if [ "$MODE" = "binary" ]; then
  mkdir -p "$BINDIR"
  echo "正在从 Hub 下载 Agent（$ARCH）..."
  # 先落临时文件：下载中断会在磁盘上留半个可执行文件，下次容器启动直接崩，
  # 而且报错信息跟「下载失败」毫无关系，极难查。成功之后再改名。
  if ! curl -fsSL -H "X-Agent-Token: $TOKEN" "$HUB_URL/api/agent/binary?arch=$ARCH" -o "$BINDIR/autodocker.new"; then
    rm -f "$BINDIR/autodocker.new"
    echo "" >&2
    echo "从 Hub 下载 Agent 失败。常见原因：" >&2
    echo "  1. 这台机器的架构（$ARCH）和 Hub 不同，没法直接下发二进制" >&2
    echo "  2. Hub 地址不通，或这条命令里的令牌已经失效" >&2
    echo "" >&2
    echo "如果两边架构不同，改用镜像方式重试（末尾加 --image）：" >&2
    echo "  curl -fsSL %s/api/agent/install.sh | sh -s -- --token=$TOKEN --image" >&2
    exit 1
  fi
  chmod +x "$BINDIR/autodocker.new"
  mv "$BINDIR/autodocker.new" "$BINDIR/autodocker"
  echo "已就位：$BINDIR/autodocker"
fi

if [ "$MODE" = "image" ]; then
  echo "正在拉取镜像 $IMAGE ..."
  # 拉不动不算失败：国内很多 NAS 上 Docker Hub 需要代理，
  # 而镜像可能早就手动导入过（或者用的是私有加速器）。
  if ! docker pull "$IMAGE"; then
    echo "拉取失败，检查本地是否已有该镜像 ..." >&2
    if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
      echo "本地也没有 $IMAGE。请先解决镜像拉取（代理 / 加速器），或手动导入后重试。" >&2
      exit 1
    fi
    echo "本地已有 $IMAGE，继续使用。" >&2
  fi
else
  # 二进制方式仍然需要一个最小 rootfs 来跑，不能指望宿主机的动态库。
  # alpine 只有 3MB，绝大多数机器上已经有或者拉得动。
  if ! docker image inspect "$BASE_IMAGE" >/dev/null 2>&1; then
    echo "正在准备基础镜像 $BASE_IMAGE ..."
    docker pull "$BASE_IMAGE" >/dev/null 2>&1 || {
      echo "拉不到 $BASE_IMAGE，可改用镜像方式：在命令末尾加 --image" >&2
      exit 1
    }
  fi
fi

if docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "$CONTAINER"; then
  echo "检测到已有 Agent，正在替换（数据卷 $VOLUME 保留）"
  docker rm -f "$CONTAINER" >/dev/null
fi

NODE_NAME=$(hostname 2>/dev/null || echo unknown)

if [ "$MODE" = "image" ]; then
  docker run -d --name "$CONTAINER" --restart=unless-stopped \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$VOLUME":/data \
    -e AUTODOCKER_MODE=agent \
    -e AUTODOCKER_HUB="$HUB_URL" \
    -e AUTODOCKER_TOKEN="$TOKEN" \
    -e AUTODOCKER_NODE_NAME="$NODE_NAME" \
    "$IMAGE" >/dev/null
else
  docker run -d --name "$CONTAINER" --restart=unless-stopped \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$VOLUME":/data \
    -v "$BINDIR/autodocker":/app/autodocker:ro \
    --entrypoint /app/autodocker \
    -e AUTODOCKER_MODE=agent \
    -e AUTODOCKER_HUB="$HUB_URL" \
    -e AUTODOCKER_TOKEN="$TOKEN" \
    -e AUTODOCKER_NODE_NAME="$NODE_NAME" \
    "$BASE_IMAGE" >/dev/null
fi

echo ""
echo "节点已启动（名称：$NODE_NAME）。回到 Hub 页面点「刷新」即可看到。"
echo "查看日志：docker logs -f $CONTAINER"
echo "升级：重跑同一条命令（数据卷保留）；Hub 升级后重跑一次，Agent 版本会跟着对齐。"
echo "卸载：docker rm -f $CONTAINER && docker volume rm $VOLUME"
`, shQuote(hubURL), shQuote(image), strings.TrimRight(hubURL, "/"), strings.TrimRight(hubURL, "/"))
}

// shQuote 用单引号包裹，避免地址里的特殊字符被 shell 解释。
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

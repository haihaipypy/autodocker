// Package hubproto 定义 Hub 与 Agent 之间的 WebSocket 帧格式。
//
// 单独成包是为了让两侧（internal/hub、internal/hubserver）共用同一份结构，
// 避免各写一份 JSON 字段名、升级时对不上。
//
// 设计上刻意保持"薄"：帧只负责搬运，不含任何业务判断。谁该扫、扫完怎么判定、
// 通知发不发，全部留在各自的业务包里。
package hubproto

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Protocol 当前协议版本。Hub 见到不认识的版本会明确报错，
// 而不是用旧结构去解新数据 —— 那样出的问题极难排查。
//
// 加新指令 / 新帧类型**不**动这个数字：它们都是纯增量，老版本收到不认识的东西
// 要么原样丢弃（帧类型）、要么 ack 一条「不支持的指令」（action）。
// 升版本意味着所有已装好的 Agent 当场被踢下线，代价远大于收益。
// 能力差异靠「这条 action 有没有被 ack 成功」来判定。
const Protocol = 1

// 帧类型。
const (
	TypeHello   = "hello"    // Agent → Hub，注册
	TypeHelloOK = "hello_ok" // Hub → Agent，注册成功
	TypeReport  = "report"   // Agent → Hub，巡检结果
	TypeCmd     = "cmd"      // Hub → Agent，指令
	TypeCmdAck  = "cmd_ack"  // Agent → Hub，指令结果
	TypeEvent   = "event"    // Agent → Hub，耗时操作的中间状态
	TypePing    = "ping"     // Hub → Agent，心跳
	TypePong    = "pong"     // Agent → Hub，心跳回应
	TypeFatal   = "fatal"    // Hub → Agent，不可恢复的错误（如协议不兼容）
)

// 指令动作。白名单之外的任何字符串都会被 Agent 拒绝。
const (
	ActionScan = "scan"
	// ActionUpdate 重建容器，过程通过 event 帧回传
	ActionUpdate = "update"
	// ActionUpgrade 把自己换成 Hub 这一版（1.8.4 起支持）。
	//
	// 比 update 更"危险"：它动的是 Agent 自身容器，执行期间这台节点会掉线一次。
	// 不认识这个动作的旧 Agent 会回一句"不支持的指令"，Hub 据此提示用户
	// 去目标机重跑安装命令 —— 这也是最后一次需要手工介入的升级。
	ActionUpgrade = "upgrade"
	// ActionPull 只拉镜像，不碰任何容器（1.10.4 起支持）。
	//
	// 给批量更新做前置：先把一整个项目的镜像全部拉到位，全部成功后才开始重建。
	// 以前是「拉一个建一个」，第一个成员拉到一半失败时，前面的容器已经是新版了，
	// 一个 compose 项目就停在新旧混搭的状态上 —— 那比不更新更难收拾。
	//
	// 旧 Agent 会回"不支持的指令: pull"，Hub 认这句话，退化成逐个更新。
	ActionPull = "pull"
	// ActionConfig 下发 Hub 侧的运行配置（1.12.0 起）。
	//
	// 目前只有一项：镜像加速地址。节点的检测由 Agent 自己发 HTTP 请求，
	// 不经过 Docker 守护进程，所以 Hub 里填好的加速源它一个都用不上 ——
	// 宿主机能拉镜像、节点页却整列「未知」，根子就在这里。
	//
	// 老 Agent 会回"不支持的指令: config"，Hub 据此只记一行日志：
	// 这是纯增量能力，不该因为某一个节点落后就把整条链路当成故障。
	ActionConfig = "config"
)

// 事件阶段。一次操作的完整生命周期：start → progress* → done | failed。
//
// 单独一帧而不是靠下一次全量上报：重建容器要几十秒到几分钟，
// 这期间页面上什么都不动，用户只会反复点按钮。
const (
	PhaseStart    = "start"
	PhaseProgress = "progress"
	PhaseDone     = "done"
	PhaseFailed   = "failed"
)

// ErrBusy Agent 拒收指令时的原话：目标上已经有一个更新 / 拉取在跑。
//
// 定成常量是为了让两头说同一句话 —— Agent 拿它回执，Hub 那边靠 IsBusy 认它。
// 「撞上另一把锁」和「镜像拉不到」必须分得开：前者等一会儿再点就行，
// 后者才要换加速源。混成一句「更新失败」，用户只会往错的方向折腾。
const ErrBusy = "已有更新任务正在执行，请稍后重试"

// BusyError 目标上另一个任务占着更新锁，这条指令没有落地。
//
// 用类型而不是一句话，是因为上层要拿它做判断：撞锁时劝人「等它跑完」，
// 只有真的拉不到镜像才该劝人「换个加速源」。这两种失败给用户的下一步
// 动作完全相反，靠字符串猜迟早猜错。
type BusyError struct {
	// Where 哪台机器（远端节点的名字）。留空表示就是当前这台。
	Where string
	// Who 正占着锁的容器名，不知道时留空。
	Who string
}

func (e *BusyError) Error() string {
	where := "这台机器"
	if e.Where != "" {
		where = "节点 " + e.Where
	}
	if e.Who == "" {
		return where + "上还有一个更新没跑完，等它结束后再试"
	}
	return where + "上正在处理 " + e.Who + "，等它结束后再试"
}

// IsBusy 判断错误是不是撞上了目标机器上的另一把锁。
//
// 除了 BusyError，还要认 Agent 的原话：升级前的老 Agent 也可能直接把
// 这句话回上来，而它同样属于「等一会儿再点」这一类。
func IsBusy(err error) bool {
	var be *BusyError
	if errors.As(err, &be) {
		return true
	}
	return err != nil && strings.Contains(err.Error(), "已有更新任务正在执行")
}

// Frame 是唯一的信封。Data 延迟解析：不认识类型时原样丢弃即可。
type Frame struct {
	Type   string          `json:"type"`
	Seq    int64           `json:"seq,omitempty"`
	ID     string          `json:"id,omitempty"`     // 指令 ID，用于请求-响应配对
	Action string          `json:"action,omitempty"` // 仅 cmd / cmd_ack
	OK     bool            `json:"ok,omitempty"`
	Error  string          `json:"error,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// Hello Agent 连上后发的第一帧。
type Hello struct {
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	AgentVersion string `json:"agent_version"`
	Protocol     int    `json:"protocol"`
	NodeName     string `json:"node_name"` // 安装时由 hostname 带入，允许为空
}

// HelloOK Hub 的接受回执。
type HelloOK struct {
	NodeID       int64  `json:"node_id"`
	NodeName     string `json:"node_name"`
	HubVersion   string `json:"hub_version"`
	ScanInterval int    `json:"scan_interval"` // 秒，0 表示沿用 Agent 自己配置

	// Mirrors 要下发到节点的镜像加速地址。
	//
	// 三态，别用「空切片等于没配」去糊：
	//   nil   —— 别动，Agent 保留自己的配置（Hub 关掉了这个开关）
	//   []    —— 清回 Agent 自己的配置（用户把加速地址删光了）
	//   [a,b] —— 整体替换成这个列表
	// 合并两份列表的方案不可取：顺序决定了回退优先级，合起来那顺序没法解释。
	Mirrors *[]string `json:"mirrors,omitempty"`
}

// Container 一条容器观测值。
//
// 字段与 model.Image 有重合但不复用：那个结构带着 size_bytes / image_created
// 这类只有本机用得上的字段，而这里是跨网络传输的契约，多一个字段就是多一份
// 升级时要兼容的负担。
type Container struct {
	ContainerID   string `json:"container_id"`
	ContainerName string `json:"container_name"`
	// ComposeProject 容器所属的 compose 项目（来自容器标签）。
	// 老 Agent 不上报这一栏，Hub 收到空串就退化成一条一条提醒。
	ComposeProject string `json:"compose_project,omitempty"`
	// ComposeService / ComposeDeps 支撑「按 depends_on 顺序重建」。
	//
	// depends_on 里写的是服务名，而容器名带项目前缀与序号，所以两个标签
	// 必须一起带上来才能把依赖关系对到具体容器。Hub 侧据此在批量更新前
	// 排一次序：上游先换、下游后换。老 Agent 不带，排不出顺序就保持原样。
	ComposeService string   `json:"compose_service,omitempty"`
	ComposeDeps    []string `json:"compose_deps,omitempty"`
	ImageRef       string   `json:"image_ref"`
	ImageID        string   `json:"image_id"`
	LocalTag       string   `json:"local_tag"`
	LocalDigest    string   `json:"local_digest"`
	RemoteDigest   string   `json:"remote_digest"`
	LatestTag      string   `json:"latest_tag"`
	Aliases        []string `json:"aliases,omitempty"`
	Origin         string   `json:"origin"`
	Status         string   `json:"status"`
	Mode           string   `json:"mode"`
	Ignored        bool     `json:"ignored"`
	LastError      string   `json:"last_error,omitempty"`
	CheckedAt      int64    `json:"checked_at"` // unix 秒
}

// Report 一次巡检的完整结果。
//
// Containers 是**全量**而不是增量：Hub 收到后整体替换该节点的快照。
// 增量同步要处理容器被删、改名、镜像换掉一堆边界，而一份快照本来就只有几十 KB。
type Report struct {
	ScannedAt  int64       `json:"scanned_at"`
	Containers []Container `json:"containers"`
	ScanError  string      `json:"scan_error,omitempty"`
	// Logs 是最近若干行日志，让用户在 Hub 页面上就能看出节点出了什么事，
	// 不用 SSH 登进去翻 docker logs。
	Logs []string `json:"logs,omitempty"`
	// Upgrade 上一次自动升级的结局，只在升级后第一次上报时带一次。
	//
	// 为什么不靠"版本变了没有"推断：helper 回滚成功时节点版本没变，
	// 但那不是"什么都没发生" —— 用户需要知道失败原因，否则只会看到
	// 节点莫名其妙掉线几秒，然后一切照旧。
	Upgrade *UpgradeResult `json:"upgrade,omitempty"`
}

// UpgradeResult 一次自动升级的结局。
type UpgradeResult struct {
	OK     bool   `json:"ok"`
	From   string `json:"from,omitempty"`
	Target string `json:"target"`
	Error  string `json:"error,omitempty"`
	At     int64  `json:"at,omitempty"`
}

// Cmd 下发给 Agent 的指令。
type Cmd struct {
	// scan 用：ContainerID 留空表示全量巡检；update 用：指定要重建哪个容器。
	ContainerID   string `json:"container_id,omitempty"`
	ContainerName string `json:"container_name,omitempty"`
	ImageRef      string `json:"image_ref,omitempty"`
	Force         bool   `json:"force,omitempty"` // scan 用：忽略去重

	// upgrade 用：目标版本与（镜像方式安装时）要拉的目标镜像。
	TargetVersion string `json:"target_version,omitempty"`

	// config 用：要下发的镜像加速地址（整列表替换）。
	//
	// 用 nil 与空切片区分「没说」和「清空」：前者是别的指令顺手带来的零值，
	// 后者是用户真的把加速地址删光了。混为一谈会让节点悄悄回退到内置默认源，
	// 而页面上明明显示「已清空」。
	Mirrors *[]string `json:"mirrors,omitempty"`
}

// Event 一次耗时操作的中间状态。
//
// OpID 把同一次操作的若干帧串起来：用户可能在页面上连点两次，
// 或者上一轮的超时通知晚到，没有这个 ID 就分不清哪条属于哪次。
type Event struct {
	OpID          string `json:"op_id"`
	Kind          string `json:"kind"` // 目前只有 update
	Phase         string `json:"phase"`
	ContainerID   string `json:"container_id,omitempty"`
	ContainerName string `json:"container_name,omitempty"`
	Message       string `json:"message,omitempty"`
	TaskID        int64  `json:"task_id,omitempty"`
	At            int64  `json:"at"`
}

// CmdAck 指令执行结果。
type CmdAck struct {
	Message string `json:"message,omitempty"`
}

// Now 便于两侧统一时间口径。
func Now() int64 { return time.Now().Unix() }

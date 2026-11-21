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
	"time"
)

// Protocol 当前协议版本。Hub 见到不认识的版本会明确报错，
// 而不是用旧结构去解新数据 —— 那样出的问题极难排查。
const Protocol = 1

// 帧类型。
const (
	TypeHello   = "hello"    // Agent → Hub，注册
	TypeHelloOK = "hello_ok" // Hub → Agent，注册成功
	TypeReport  = "report"   // Agent → Hub，巡检结果
	TypeCmd     = "cmd"      // Hub → Agent，指令
	TypeCmdAck  = "cmd_ack"  // Agent → Hub，指令结果
	TypePing    = "ping"     // Hub → Agent，心跳
	TypePong    = "pong"     // Agent → Hub，心跳回应
	TypeFatal   = "fatal"    // Hub → Agent，不可恢复的错误（如协议不兼容）
)

// 指令动作。白名单之外的任何字符串都会被 Agent 拒绝。
const (
	ActionScan = "scan"
)

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
}

// Container 一条容器观测值。
//
// 字段与 model.Image 有重合但不复用：那个结构带着 size_bytes / image_created
// 这类只有本机用得上的字段，而这里是跨网络传输的契约，多一个字段就是多一份
// 升级时要兼容的负担。
type Container struct {
	ContainerID   string   `json:"container_id"`
	ContainerName string   `json:"container_name"`
	ImageRef      string   `json:"image_ref"`
	ImageID       string   `json:"image_id"`
	LocalTag      string   `json:"local_tag"`
	LocalDigest   string   `json:"local_digest"`
	RemoteDigest  string   `json:"remote_digest"`
	LatestTag     string   `json:"latest_tag"`
	Aliases       []string `json:"aliases,omitempty"`
	Origin        string   `json:"origin"`
	Status        string   `json:"status"`
	Mode          string   `json:"mode"`
	Ignored       bool     `json:"ignored"`
	LastError     string   `json:"last_error,omitempty"`
	CheckedAt     int64    `json:"checked_at"` // unix 秒
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
}

// Cmd 下发给 Agent 的指令。
type Cmd struct {
	ContainerID string `json:"container_id,omitempty"`
	Force       bool   `json:"force,omitempty"` // scan 用：忽略去重
}

// CmdAck 指令执行结果。
type CmdAck struct {
	Message string `json:"message,omitempty"`
}

// Now 便于两侧统一时间口径。
func Now() int64 { return time.Now().Unix() }

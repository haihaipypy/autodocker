package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/haihaipypy/autodocker/internal/model"
)

const nodeCols = `id, name, hub_url, note, hostname, os, arch, agent_version,
	protocol, status, last_seen_at, last_report_at, created_at`

func scanNode(sc rowScanner) (model.Node, error) {
	var (
		n          model.Node
		lastSeen   int64
		lastReport int64
		createdAt  int64
	)
	err := sc.Scan(
		&n.ID, &n.Name, &n.HubURL, &n.Note, &n.Hostname, &n.OS, &n.Arch,
		&n.AgentVersion, &n.Protocol, &n.Status, &lastSeen, &lastReport, &createdAt,
	)
	if err != nil {
		return n, err
	}
	n.LastSeenAt = tsToTime(lastSeen)
	n.LastReportAt = tsToTime(lastReport)
	n.CreatedAt = tsToTime(createdAt)
	if n.Status == "" {
		n.Status = model.NodeNever
	}
	return n, nil
}

// CreateNode 新建一个节点，返回 ID。tokenHash 是令牌的 sha256，明文不落库。
func (s *Store) CreateNode(n *model.Node, tokenHash string) (int64, error) {
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO nodes (name, hub_url, note, token_hash, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		n.Name, n.HubURL, n.Note, tokenHash, model.NodeNever, now.Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("创建节点失败: %w", err)
	}
	return res.LastInsertId()
}

// ListNodes 返回全部节点，附带容器数与待更新数。
//
// 两个统计值用子查询现算而不是存成列：容器快照每轮都整体替换，
// 存冗余列就得在每个替换路径上记得同步，迟早会有一处漏掉。
//
// 口径必须是 origin = 'container'：远端快照里还可能躺着镜像维度的条目
// （老版本 Agent 会送），把它们数进「容器」会让节点卡片显示一个
// 谁也对不上的数字。
func (s *Store) ListNodes() ([]model.Node, error) {
	rows, err := s.db.Query(
		`SELECT ` + nodeCols + `,
			(SELECT COUNT(*) FROM node_containers c
			  WHERE c.node_id = nodes.id AND c.origin = 'container') AS container_count,
			(SELECT COUNT(*) FROM node_containers c
			  WHERE c.node_id = nodes.id
			    AND c.origin = 'container'
			    AND c.status IN ('update_available', 'new_version_tag')) AS update_count
		 FROM nodes ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.Node
	for rows.Next() {
		var (
			n                   model.Node
			lastSeen, createdAt int64
			lastReport          int64
			containerCount      int
			updateCount         int
		)
		if err := rows.Scan(
			&n.ID, &n.Name, &n.HubURL, &n.Note, &n.Hostname, &n.OS, &n.Arch,
			&n.AgentVersion, &n.Protocol, &n.Status, &lastSeen, &lastReport, &createdAt,
			&containerCount, &updateCount,
		); err != nil {
			return nil, err
		}
		n.LastSeenAt = tsToTime(lastSeen)
		n.LastReportAt = tsToTime(lastReport)
		n.CreatedAt = tsToTime(createdAt)
		n.ContainerCount = containerCount
		n.UpdateCount = updateCount
		if n.Status == "" {
			n.Status = model.NodeNever
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NodeUpdateTotals 汇总所有节点快照里「可更新」的容器数。
//
// 口径与节点卡片上的 update_count 完全一致（容器维度、两种更新状态都算），
// 这样首页那个数字跟节点页每张卡上的「待更新 N」加得起来 —— 两边对不上时，
// 看的人会先怀疑自己数错了，然后不再相信任何一个数字。
//
// 离线节点的快照也计入：那是它最后一次上报时确实还欠着更新的数量，
// 排除掉只会让首页数字在节点掉线时莫名其妙变小，看着像更新自己消失了。
func (s *Store) NodeUpdateTotals() (containers, updates int, err error) {
	err = s.db.QueryRow(
		`SELECT
			(SELECT COUNT(*) FROM node_containers
			  WHERE origin = 'container'),
			(SELECT COUNT(*) FROM node_containers
			  WHERE origin = 'container'
			    AND status IN ('update_available', 'new_version_tag'))`,
	).Scan(&containers, &updates)
	return containers, updates, err
}

// GetNode 按 ID 查节点。
func (s *Store) GetNode(id int64) (*model.Node, error) {
	row := s.db.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id = ?`, id)
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// FindNodeByTokenHash 用令牌摘要找节点，用于 WS 连接鉴权。
func (s *Store) FindNodeByTokenHash(hash string) (*model.Node, error) {
	if hash == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE token_hash = ?`, hash)
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// UpdateNodeHello 记录 Agent 注册时上报的身份信息，并置为在线。
func (s *Store) UpdateNodeHello(id int64, hostname, osName, arch, version string, protocol int) error {
	_, err := s.db.Exec(
		`UPDATE nodes SET hostname = ?, os = ?, arch = ?, agent_version = ?,
			protocol = ?, status = ?, last_seen_at = ?
		 WHERE id = ?`,
		hostname, osName, arch, version, protocol, model.NodeOnline, time.Now().Unix(), id,
	)
	return err
}

// TouchNode 收到心跳时刷新在线状态。
func (s *Store) TouchNode(id int64) error {
	_, err := s.db.Exec(
		`UPDATE nodes SET status = ?, last_seen_at = ? WHERE id = ?`,
		model.NodeOnline, time.Now().Unix(), id,
	)
	return err
}

// SetNodeReported 记下「刚收到一份完整快照」。
//
// 只有落库成功才调用：上报解析失败或写库失败时不能盖这个戳，
// 否则页面会显示一份不知道从哪来的旧数据。
func (s *Store) SetNodeReported(id int64) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(
		`UPDATE nodes SET last_report_at = ?, last_seen_at = ?, status = ? WHERE id = ?`,
		now, now, model.NodeOnline, id,
	)
	return err
}

// MarkNodeOffline 连接断开时置为离线。
func (s *Store) MarkNodeOffline(id int64) error {
	_, err := s.db.Exec(
		`UPDATE nodes SET status = ? WHERE id = ?`, model.NodeOffline, id,
	)
	return err
}

// MarkAllNodesOffline 启动时调用：进程重启后旧的「在线」状态全部作废。
//
// 只改状态，**不动** offline_alert_at：那是「这次失联已经告诉过用户了」的记号。
// 一次失联只报一次，直到节点真的回来才清 —— Hub 自己重启不该让同一次失联
// 再报一遍，用户收到两条一模一样的「节点离线」只会以为出了新问题。
func (s *Store) MarkAllNodesOffline() error {
	_, err := s.db.Exec(
		`UPDATE nodes SET status = ? WHERE status = ?`, model.NodeOffline, model.NodeOnline,
	)
	return err
}

// SetNodeLastSeen 直接改「最后一次联系」的时间。
//
// 正常路径由心跳与上报刷新，这里是留给「造历史数据」的：迁移旧库、
// 复现失联场景、测试里模拟一台两小时没回应的机器。生产代码不该在
// 心跳之外的地方调用它 —— 两个写入点会让这个时间戳变得不可信。
func (s *Store) SetNodeLastSeen(id int64, at time.Time) error {
	_, err := s.db.Exec(`UPDATE nodes SET last_seen_at = ? WHERE id = ?`, at.Unix(), id)
	return err
}

// NodeOfflineAlertAt 返回本次失联已经报过警的时刻，0 表示还没报。
func (s *Store) NodeOfflineAlertAt(id int64) (int64, error) {
	var at sql.NullInt64
	err := s.db.QueryRow(`SELECT offline_alert_at FROM nodes WHERE id = ?`, id).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return at.Int64, nil
}

// SetNodeOfflineAlert 记下「这次失联已经报过了」。
func (s *Store) SetNodeOfflineAlert(id int64, at int64) error {
	_, err := s.db.Exec(`UPDATE nodes SET offline_alert_at = ? WHERE id = ?`, at, id)
	return err
}

// ClearNodeOfflineAlert 节点回来了，把失联记号清掉。
func (s *Store) ClearNodeOfflineAlert(id int64) error {
	_, err := s.db.Exec(`UPDATE nodes SET offline_alert_at = 0 WHERE id = ?`, id)
	return err
}

// UpdateNodeMeta 改名字与备注。
func (s *Store) UpdateNodeMeta(id int64, name, note string) error {
	_, err := s.db.Exec(`UPDATE nodes SET name = ?, note = ? WHERE id = ?`, name, note, id)
	return err
}

// SetNodeHubURL 记录这次生成安装命令时用的地址。
func (s *Store) SetNodeHubURL(id int64, hubURL string) error {
	_, err := s.db.Exec(`UPDATE nodes SET hub_url = ? WHERE id = ?`, hubURL, id)
	return err
}

// RotateNodeToken 换令牌，旧令牌立刻失效。
func (s *Store) RotateNodeToken(id int64, tokenHash string) error {
	_, err := s.db.Exec(`UPDATE nodes SET token_hash = ? WHERE id = ?`, tokenHash, id)
	return err
}

// DeleteNode 删除节点及其快照。
func (s *Store) DeleteNode(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM node_containers WHERE node_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM nodes WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceNodeContainers 用一次上报的内容整体替换该节点的快照。
//
// 放在一个事务里：中途失败时宁可保留上一轮的旧快照，也不要出现「删了一半」
// 那种既没有旧数据也没有新数据的状态 —— 页面上会突然空一片。
func (s *Store) ReplaceNodeContainers(nodeID int64, list []model.NodeContainer) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM node_containers WHERE node_id = ?`, nodeID); err != nil {
		return err
	}
	stmt, err := tx.Prepare(
		`INSERT INTO node_containers
			(node_id, container_id, container_name, compose_project, compose_service, compose_deps,
			 image_ref, image_id,
			 local_tag, local_digest, remote_digest, latest_tag, aliases,
			 origin, status, mode, ignored, last_error, checked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, c := range list {
		aliases := ""
		if len(c.Aliases) > 0 {
			b, _ := json.Marshal(c.Aliases)
			aliases = string(b)
		}
		// 依赖列表存 JSON 数组而不是逗号拼接：服务名里理论上不会有逗号，
		// 但空列表与「一个空字符串」在拼接下长得一样，JSON 不会。
		deps := "[]"
		if len(c.ComposeDeps) > 0 {
			if b, err := json.Marshal(c.ComposeDeps); err == nil {
				deps = string(b)
			}
		}
		if _, err := stmt.Exec(
			nodeID, c.ContainerID, c.ContainerName, c.ComposeProject, c.ComposeService, deps,
			c.ImageRef, c.ImageID,
			c.LocalTag, c.LocalDigest, c.RemoteDigest, c.LatestTag, aliases,
			c.Origin, c.Status, c.Mode, b2i(c.Ignored), c.LastError, timeToTs(c.CheckedAt),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListNodeContainers 读某个节点的快照。
// ListNodeContainers 读某台机器的全部容器快照。
func (s *Store) ListNodeContainers(nodeID int64) ([]model.NodeContainer, error) {
	rows, err := s.db.Query(
		`SELECT node_id, container_id, container_name, compose_project, compose_service, compose_deps, image_ref, image_id,
			local_tag, local_digest, remote_digest, latest_tag, aliases,
			origin, status, mode, ignored, last_error, checked_at
		 FROM node_containers WHERE node_id = ? ORDER BY container_name`,
		nodeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodeContainers(rows)
}

// FindNodeContainer 取某台机器上一个容器的快照。
//
// 卡片上点「更新」时只有通知里的容器编号，要拼出下发的指令（容器名、镜像引用）
// 得回到快照里查一次。查不到返回 ErrNotFound —— 那说明容器已经被删掉了，
// 这时候什么都别做比发一条注定失败的指令更好。
func (s *Store) FindNodeContainer(nodeID int64, containerID string) (*model.NodeContainer, error) {
	return s.findNodeContainer(`node_id = ? AND container_id = ?`, nodeID, containerID)
}

// FindNodeContainerByName 按容器名在这台机器的快照里找。
//
// 容器编号只在「这一个容器实例」的生命周期里有效：compose 重建、外部升级、
// 甚至一次失败的更新回滚，都可能让编号变掉，而提醒里存的是发提醒那一刻的编号。
// 编号对不上时按名字再找一次 —— 名字是用户在页面上真正认得的东西，重建时也保留。
func (s *Store) FindNodeContainerByName(nodeID int64, name string) (*model.NodeContainer, error) {
	if name == "" {
		return nil, ErrNotFound
	}
	return s.findNodeContainer(`node_id = ? AND container_name = ?`, nodeID, name)
}

func (s *Store) findNodeContainer(where string, args ...any) (*model.NodeContainer, error) {
	row := s.db.QueryRow(
		`SELECT node_id, container_id, container_name, compose_project, compose_service, compose_deps, image_ref, image_id,
			local_tag, local_digest, remote_digest, latest_tag, aliases,
			origin, status, mode, ignored, last_error, checked_at
		 FROM node_containers WHERE `+where+` LIMIT 1`,
		args...,
	)
	var (
		c         model.NodeContainer
		aliases   string
		depsJSON  string
		ignored   int
		checkedAt int64
	)
	err := row.Scan(
		&c.NodeID, &c.ContainerID, &c.ContainerName, &c.ComposeProject, &c.ComposeService, &depsJSON,
		&c.ImageRef, &c.ImageID,
		&c.LocalTag, &c.LocalDigest, &c.RemoteDigest, &c.LatestTag, &aliases,
		&c.Origin, &c.Status, &c.Mode, &ignored, &c.LastError, &checkedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.Ignored = ignored != 0
	c.CheckedAt = tsToTime(checkedAt)
	if aliases != "" {
		_ = json.Unmarshal([]byte(aliases), &c.Aliases)
	}
	if depsJSON != "" {
		_ = json.Unmarshal([]byte(depsJSON), &c.ComposeDeps)
	}
	return &c, nil
}

// ListAllNodeContainers 读所有节点的快照，附带节点名。
//
// 列表页的「全部节点」视图就靠它。节点被删掉时快照会一起删（见 DeleteNode），
// 这里仍然用 JOIN 而不是拼内存，避免出现孤儿行。
func (s *Store) ListAllNodeContainers() ([]model.NodeContainerView, error) {
	rows, err := s.db.Query(
		`SELECT c.node_id, c.container_id, c.container_name, c.compose_project,
			c.compose_service, c.compose_deps, c.image_ref, c.image_id,
			c.local_tag, c.local_digest, c.remote_digest, c.latest_tag, c.aliases,
			c.origin, c.status, c.mode, c.ignored, c.last_error, c.checked_at,
			n.name
		 FROM node_containers c
		 JOIN nodes n ON n.id = c.node_id
		 ORDER BY n.id, c.container_name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.NodeContainerView
	for rows.Next() {
		var (
			c         model.NodeContainer
			aliases   string
			depsJSON  string
			ignored   int
			checkedAt int64
			nodeName  string
		)
		if err := rows.Scan(
			&c.NodeID, &c.ContainerID, &c.ContainerName, &c.ComposeProject, &c.ComposeService, &depsJSON,
			&c.ImageRef, &c.ImageID,
			&c.LocalTag, &c.LocalDigest, &c.RemoteDigest, &c.LatestTag, &aliases,
			&c.Origin, &c.Status, &c.Mode, &ignored, &c.LastError, &checkedAt,
			&nodeName,
		); err != nil {
			return nil, err
		}
		c.Ignored = ignored != 0
		c.CheckedAt = tsToTime(checkedAt)
		if aliases != "" {
			_ = json.Unmarshal([]byte(aliases), &c.Aliases)
		}
		if depsJSON != "" {
			_ = json.Unmarshal([]byte(depsJSON), &c.ComposeDeps)
		}
		out = append(out, model.NodeContainerView{NodeContainer: c, NodeName: nodeName})
	}
	return out, rows.Err()
}

func scanNodeContainers(rows *sql.Rows) ([]model.NodeContainer, error) {
	var out []model.NodeContainer
	for rows.Next() {
		var (
			c         model.NodeContainer
			aliases   string
			depsJSON  string
			ignored   int
			checkedAt int64
		)
		if err := rows.Scan(
			&c.NodeID, &c.ContainerID, &c.ContainerName, &c.ComposeProject, &c.ComposeService, &depsJSON,
			&c.ImageRef, &c.ImageID,
			&c.LocalTag, &c.LocalDigest, &c.RemoteDigest, &c.LatestTag, &aliases,
			&c.Origin, &c.Status, &c.Mode, &ignored, &c.LastError, &checkedAt,
		); err != nil {
			return nil, err
		}
		c.Ignored = ignored != 0
		c.CheckedAt = tsToTime(checkedAt)
		if aliases != "" {
			_ = json.Unmarshal([]byte(aliases), &c.Aliases)
		}
		if depsJSON != "" {
			_ = json.Unmarshal([]byte(depsJSON), &c.ComposeDeps)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

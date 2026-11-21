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
func (s *Store) MarkAllNodesOffline() error {
	_, err := s.db.Exec(
		`UPDATE nodes SET status = ? WHERE status = ?`, model.NodeOffline, model.NodeOnline,
	)
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
			(node_id, container_id, container_name, image_ref, image_id,
			 local_tag, local_digest, remote_digest, latest_tag, aliases,
			 origin, status, mode, ignored, last_error, checked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
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
		if _, err := stmt.Exec(
			nodeID, c.ContainerID, c.ContainerName, c.ImageRef, c.ImageID,
			c.LocalTag, c.LocalDigest, c.RemoteDigest, c.LatestTag, aliases,
			c.Origin, c.Status, c.Mode, b2i(c.Ignored), c.LastError, timeToTs(c.CheckedAt),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListNodeContainers 读某个节点的快照。
func (s *Store) ListNodeContainers(nodeID int64) ([]model.NodeContainer, error) {
	rows, err := s.db.Query(
		`SELECT node_id, container_id, container_name, image_ref, image_id,
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

// ListAllNodeContainers 读所有节点的快照，附带节点名。
//
// 列表页的「全部节点」视图就靠它。节点被删掉时快照会一起删（见 DeleteNode），
// 这里仍然用 JOIN 而不是拼内存，避免出现孤儿行。
func (s *Store) ListAllNodeContainers() ([]model.NodeContainerView, error) {
	rows, err := s.db.Query(
		`SELECT c.node_id, c.container_id, c.container_name, c.image_ref, c.image_id,
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
			ignored   int
			checkedAt int64
			nodeName  string
		)
		if err := rows.Scan(
			&c.NodeID, &c.ContainerID, &c.ContainerName, &c.ImageRef, &c.ImageID,
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
			ignored   int
			checkedAt int64
		)
		if err := rows.Scan(
			&c.NodeID, &c.ContainerID, &c.ContainerName, &c.ImageRef, &c.ImageID,
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
		out = append(out, c)
	}
	return out, rows.Err()
}

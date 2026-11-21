package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/haihaipypy/autodocker/internal/model"
)

const notifCols = `id, image_ref, container_id, container_name, kind, title, body,
	local_summary, remote_summary, local_tag, latest_tag, aliases, read,
	channel, chat_ref, message_id, card_state, deliveries,
	created_at, updated_at`

func scanNotification(sc rowScanner) (model.Notification, error) {
	var (
		n              model.Notification
		aliasesJSON    string
		deliveriesJSON string
		read           int
		channel        string
		createdAt      int64
		updatedAt      int64
	)
	err := sc.Scan(
		&n.ID, &n.ImageRef, &n.ContainerID, &n.ContainerName, &n.Kind, &n.Title, &n.Body,
		&n.LocalSummary, &n.RemoteSummary, &n.LocalTag, &n.LatestTag, &aliasesJSON, &read,
		&channel, &n.ChatRef, &n.MessageID, &n.CardState, &deliveriesJSON, &createdAt, &updatedAt,
	)
	if err != nil {
		return n, err
	}
	n.Channel = model.Channel(channel)
	if n.Channel == "" {
		n.Channel = model.ChannelFeishu
	}
	n.Read = read != 0
	n.CreatedAt = tsToTime(createdAt)
	n.UpdatedAt = tsToTime(updatedAt)
	if aliasesJSON != "" {
		_ = json.Unmarshal([]byte(aliasesJSON), &n.Aliases)
	}
	if deliveriesJSON != "" {
		_ = json.Unmarshal([]byte(deliveriesJSON), &n.Deliveries)
	}
	return n, nil
}

// CreateNotification 写入一条通知记录。
func (s *Store) CreateNotification(n *model.Notification) (int64, error) {
	now := time.Now()
	if n.CardState == "" {
		n.CardState = model.CardPending
	}
	if n.CreatedAt.IsZero() {
		n.CreatedAt = now
	}
	aliases, err := json.Marshal(n.Aliases)
	if err != nil {
		aliases = []byte("[]")
	}
	deliveries, derr := json.Marshal(n.Deliveries)
	if derr != nil || len(n.Deliveries) == 0 {
		deliveries = []byte("[]")
	}
	var id int64
	err = s.db.QueryRow(`
		INSERT INTO notifications(
			image_ref, container_id, container_name, kind, title, body,
			local_summary, remote_summary, local_tag, latest_tag, aliases, read,
			channel, chat_ref, message_id, card_state, deliveries, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		RETURNING id`,
		n.ImageRef, n.ContainerID, n.ContainerName, string(n.Kind), n.Title, n.Body,
		n.LocalSummary, n.RemoteSummary, n.LocalTag, n.LatestTag, string(aliases), b2i(n.Read),
		string(n.Channel), n.ChatRef, n.MessageID, string(n.CardState), string(deliveries),
		timeToTs(n.CreatedAt), timeToTs(now),
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	n.ID = id
	return id, nil
}

// GetNotification 按主键读取。
func (s *Store) GetNotification(id int64) (*model.Notification, error) {
	n, err := scanNotification(s.db.QueryRow(`SELECT `+notifCols+` FROM notifications WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// ListNotifications 列出通知，未读优先、时间倒序。
func (s *Store) ListNotifications(unreadOnly bool, limit int) ([]model.Notification, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + notifCols + ` FROM notifications`
	if unreadOnly {
		query += ` WHERE read = 0`
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`

	rows, err := s.db.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Notification{}
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// FindPendingNotification 找出某个镜像尚未处理的提醒，用于避免重复推送。
func (s *Store) FindPendingNotification(imageRef, containerID string) (*model.Notification, error) {
	n, err := scanNotification(s.db.QueryRow(
		`SELECT `+notifCols+` FROM notifications
		 WHERE image_ref = ? AND container_id = ? AND card_state IN (?, ?)
		 ORDER BY created_at DESC LIMIT 1`,
		imageRef, containerID, string(model.CardPending), string(model.CardUpdating),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// HasNotificationForTag 判断某个具体版本 tag 是否已经通知过。
//
// 排除 snoozed：那是用户点了「稍后提醒」，语义是「下次巡检再提我一遍」。
// 把它算成「已经通知过」，那张卡片就会变成最后一次提醒，之后再无消息。
func (s *Store) HasNotificationForTag(imageRef, containerID, tag string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM notifications WHERE image_ref = ? AND container_id = ? AND latest_tag = ?
			AND card_state <> ?`,
		imageRef, containerID, tag, string(model.CardSnoozed),
	).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkNotificationRead 标记单条已读/未读。
func (s *Store) MarkNotificationRead(id int64, read bool) error {
	_, err := s.db.Exec(`UPDATE notifications SET read = ?, updated_at = ? WHERE id = ?`,
		b2i(read), timeToTs(time.Now()), id)
	return err
}

// MarkAllRead 全部标记已读。
func (s *Store) MarkAllRead() error {
	_, err := s.db.Exec(`UPDATE notifications SET read = 1, updated_at = ? WHERE read = 0`, timeToTs(time.Now()))
	return err
}

// SetNotificationDeliveries 写入全部投递记录，并把第一条当作主投递。
//
// 主投递（channel/chat_ref/message_id）留着是给前端展示和兼容老数据用的；
// 全量列表才是「更新完成后要去改哪几条消息」的真实依据 ——
// 少记一条，那个渠道的卡片就会永远停在「待更新」。
func (s *Store) SetNotificationDeliveries(id int64, list []model.Delivery, state model.CardState) error {
	if len(list) == 0 {
		return nil
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	first := list[0]
	_, err = s.db.Exec(
		`UPDATE notifications SET deliveries = ?, channel = ?, chat_ref = ?, message_id = ?,
		 card_state = ?, updated_at = ? WHERE id = ?`,
		string(raw), string(first.Channel), first.ChatRef, first.MessageID,
		string(state), timeToTs(time.Now()), id,
	)
	return err
}

// UpdateNotificationDeliveries 只改投递列表（用于 Telegram 点按钮时纠正会话 ID）。
func (s *Store) UpdateNotificationDeliveries(id int64, list []model.Delivery) error {
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE notifications SET deliveries = ?, updated_at = ? WHERE id = ?`,
		string(raw), timeToTs(time.Now()), id)
	return err
}

// SetNotificationChatRef 补记会话 ID。
// Telegram 的消息 ID 只在「会话 + 消息」这对坐标里有意义：
// 通知是按默认会话发出去的，但用户可能在另一个群里点按钮，
// 之后改写消息必须用他点击时所在的那个会话。
func (s *Store) SetNotificationChatRef(id int64, chatRef string) error {
	_, err := s.db.Exec(`UPDATE notifications SET chat_ref = ?, updated_at = ? WHERE id = ?`,
		chatRef, timeToTs(time.Now()), id)
	return err
}

// SetNotificationCardState 只更新交互状态。
func (s *Store) SetNotificationCardState(id int64, state model.CardState) error {
	_, err := s.db.Exec(`UPDATE notifications SET card_state = ?, updated_at = ? WHERE id = ?`,
		string(state), timeToTs(time.Now()), id)
	return err
}

// DeleteReadNotifications 清空已读通知，返回删除条数。
func (s *Store) DeleteReadNotifications() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM notifications WHERE read = 1`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountUnread 未读数量。
func (s *Store) CountUnread() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE read = 0`).Scan(&n)
	return n, err
}

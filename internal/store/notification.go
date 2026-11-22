package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/haihaipypy/autodocker/internal/model"
)

const notifCols = `id, image_ref, container_id, container_name, node_id, node_name,
	version_key, kind, title, body,
	local_summary, remote_summary, local_tag, latest_tag, aliases, read,
	channel, chat_ref, message_id, card_state, card_detail,
	compose_project, member_count, deliveries,
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
		&n.ID, &n.ImageRef, &n.ContainerID, &n.ContainerName, &n.NodeID, &n.NodeName,
		&n.VersionKey, &n.Kind, &n.Title, &n.Body,
		&n.LocalSummary, &n.RemoteSummary, &n.LocalTag, &n.LatestTag, &aliasesJSON, &read,
		&channel, &n.ChatRef, &n.MessageID, &n.CardState, &n.CardDetail,
		&n.ComposeProject, &n.MemberCount, &deliveriesJSON, &createdAt, &updatedAt,
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
// queryRower 让同一段插入语句既能跑在连接上，也能跑在事务里。
type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

// CreateNotification 写一条单容器提醒。
//
// 落库时顺手写一行成员：去重、逐容器结果、合并提醒的「一键更新」都只看
// notification_members，所有提醒形状一致，调用方不必分辨是哪种。
func (s *Store) CreateNotification(n *model.Notification) (int64, error) {
	m := model.NotificationMember{
		ContainerID:   n.ContainerID,
		ContainerName: n.ContainerName,
		ImageRef:      n.ImageRef,
		VersionKey:    n.VersionKey,
		Kind:          n.Kind,
		LocalTag:      n.LocalTag,
		LatestTag:     n.LatestTag,
		LocalSummary:  n.LocalSummary,
		RemoteSummary: n.RemoteSummary,
	}
	if m.ContainerID == "" && m.ContainerName == "" && m.ImageRef == "" {
		return createNotification(s.db, n)
	}
	return s.CreateNotificationWithMembers(n, []model.NotificationMember{m})
}

// CreateNotificationWithMembers 写一条提醒和它覆盖的容器（同一个事务）。
//
// 一条提醒覆盖多个容器时，主字段取第一个成员 —— 老页面、老日志、老测试
// 都还认那几个字段，没必要为了合并把入口全部换掉。
func (s *Store) CreateNotificationWithMembers(n *model.Notification, members []model.NotificationMember) (int64, error) {
	if len(members) == 0 {
		return createNotification(s.db, n)
	}
	n.MemberCount = len(members)
	if len(members) > 0 && (n.ContainerID == "" || n.ContainerName == "") {
		first := members[0]
		if n.ContainerID == "" {
			n.ContainerID = first.ContainerID
		}
		if n.ContainerName == "" {
			n.ContainerName = first.ContainerName
		}
		if n.ImageRef == "" {
			n.ImageRef = first.ImageRef
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	id, err := createNotification(tx, n)
	if err != nil {
		return 0, err
	}
	for _, m := range members {
		if m.State == "" {
			m.State = "pending"
		}
		if _, err := tx.Exec(
			`INSERT INTO notification_members(
				notification_id, container_id, container_name, image_ref, version_key,
				kind, local_tag, latest_tag, local_summary, remote_summary, state, error)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, m.ContainerID, m.ContainerName, m.ImageRef, m.VersionKey,
			string(m.Kind), m.LocalTag, m.LatestTag, m.LocalSummary, m.RemoteSummary, m.State, m.Error,
		); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	n.ID = id
	return id, nil
}

func createNotification(q queryRower, n *model.Notification) (int64, error) {
	now := time.Now()
	if n.CardState == "" {
		n.CardState = model.CardPending
	}
	if n.CreatedAt.IsZero() {
		n.CreatedAt = now
	}
	if n.MemberCount <= 0 {
		n.MemberCount = 1
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
	err = q.QueryRow(`
		INSERT INTO notifications(
			image_ref, container_id, container_name, node_id, node_name, version_key,
			kind, title, body,
			local_summary, remote_summary, local_tag, latest_tag, aliases, read,
			channel, chat_ref, message_id, card_state, card_detail,
			compose_project, member_count, deliveries, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		RETURNING id`,
		n.ImageRef, n.ContainerID, n.ContainerName, n.NodeID, n.NodeName, n.VersionKey,
		string(n.Kind), n.Title, n.Body,
		n.LocalSummary, n.RemoteSummary, n.LocalTag, n.LatestTag, string(aliases), b2i(n.Read),
		string(n.Channel), n.ChatRef, n.MessageID, string(n.CardState), n.CardDetail,
		n.ComposeProject, n.MemberCount, string(deliveries),
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
	if err := s.attachMembers([]*model.Notification{&n}); err != nil {
		return nil, err
	}
	return &n, nil
}

// attachMembers 一次查出这批通知的全部成员，挂回内存对象。
//
// 一条 SQL 查完再分组，而不是每条通知查一次：通知页一次列 100 条，
// 逐条查就是 100 次往返，页面上的「合并提醒」会明显变慢。
func (s *Store) attachMembers(list []*model.Notification) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]any, 0, len(list))
	ph := make([]string, 0, len(list))
	index := make(map[int64]*model.Notification, len(list))
	for _, n := range list {
		ids = append(ids, n.ID)
		ph = append(ph, "?")
		index[n.ID] = n
	}
	rows, err := s.db.Query(
		`SELECT notification_id, container_id, container_name, image_ref, version_key,
			kind, local_tag, latest_tag, local_summary, remote_summary, state, error
		 FROM notification_members WHERE notification_id IN (`+strings.Join(ph, ",")+`)
		 ORDER BY id`,
		ids...,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			nid int64
			m   model.NotificationMember
			k   string
		)
		if err := rows.Scan(&nid, &m.ContainerID, &m.ContainerName, &m.ImageRef, &m.VersionKey,
			&k, &m.LocalTag, &m.LatestTag, &m.LocalSummary, &m.RemoteSummary, &m.State, &m.Error); err != nil {
			return err
		}
		m.Kind = model.NotificationKind(k)
		if n := index[nid]; n != nil {
			n.Members = append(n.Members, m)
		}
	}
	return rows.Err()
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ptrs := make([]*model.Notification, len(out))
	for i := range out {
		ptrs[i] = &out[i]
	}
	if err := s.attachMembers(ptrs); err != nil {
		return nil, err
	}
	return out, nil
}

// FindPendingNotification 找出某个镜像尚未处理的提醒，用于避免重复推送。
func (s *Store) FindPendingNotification(imageRef, containerID string) (*model.Notification, error) {
	// 判重看成员表：一条提醒可能合并了整个 compose 项目，
	// 只比主字段（= 第一个成员）会漏掉项目里的其它容器。
	n, err := scanNotification(s.db.QueryRow(
		`SELECT `+notifCols+` FROM notifications
		 WHERE card_state IN (?, ?)
		   AND EXISTS (SELECT 1 FROM notification_members m
		               WHERE m.notification_id = notifications.id
		                 AND m.container_id = ? AND m.image_ref = ?)
		 ORDER BY created_at DESC LIMIT 1`,
		string(model.CardPending), string(model.CardUpdating), containerID, imageRef,
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
		`SELECT COUNT(*) FROM notifications
		  WHERE card_state <> ?
		    AND EXISTS (SELECT 1 FROM notification_members m
		                WHERE m.notification_id = notifications.id
		                  AND m.container_id = ? AND m.image_ref = ? AND m.latest_tag = ?)`,
		string(model.CardSnoozed), containerID, imageRef, tag,
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
//
// 说明文字一并清空：状态与那句结果说明是成对的，只换状态不清旧文字，
// 就会出现「重试成功、行尾还挂着一行上一次的失败原因」这种自相矛盾的画面。
// 需要留下说明的场景用 SetNotificationCardResult。
func (s *Store) SetNotificationCardState(id int64, state model.CardState) error {
	return s.SetNotificationCardResult(id, state, "")
}

// SetNotificationCardResult 同时写交互状态与那句结果说明。
//
// 失败原因必须落库：网页上没有别的地方存它，IM 卡片那版外观重启后就没了。
func (s *Store) SetNotificationCardResult(id int64, state model.CardState, detail string) error {
	_, err := s.db.Exec(
		`UPDATE notifications SET card_state = ?, card_detail = ?, updated_at = ? WHERE id = ?`,
		string(state), detail, timeToTs(time.Now()), id,
	)
	return err
}

// DeleteNotification 删除一条通知。
//
// 远端提醒「同一个版本只提醒一次」靠通知记录本身去重，所以删掉一条之后，
// 那一版如果仍然可更新，下一轮巡检会重新推一次。这是删的代价，
// 页面上不需要额外提示 —— 用户删的就是这条记录本身。
func (s *Store) DeleteNotification(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 成员行一起删干净：留着的话按容器反查结果时会一直捞到这条不存在的提醒。
	if _, err := tx.Exec(`DELETE FROM notification_members WHERE notification_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM notifications WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// DeleteReadNotifications 清空已读通知，返回删除条数。
func (s *Store) DeleteReadNotifications() (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`DELETE FROM notification_members WHERE notification_id IN (SELECT id FROM notifications WHERE read = 1)`,
	); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM notifications WHERE read = 1`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// CountUnread 未读数量。
func (s *Store) CountUnread() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE read = 0`).Scan(&n)
	return n, err
}

// HasNodeNotificationKey 判断某个远端容器在这一版上是否已经提醒过。
//
// 本机靠 images.notified_tag 做这件事，远端没有等价的落点：node_containers
// 每轮整体替换，任何「说过了」的标记都活不过一次上报。所以只能回头看通知记录。
//
// 排除 snoozed：那是用户点了「稍后提醒」，语义是「下次巡检再提我一遍」，
// 把它算成「已经提醒过」，卡片上那句承诺就成了空话。
func (s *Store) HasNodeNotificationKey(nodeID int64, containerID, key string) (bool, error) {
	if nodeID <= 0 || key == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRow(
		`SELECT 1 FROM notifications
		  WHERE node_id = ? AND card_state <> ?
		    AND EXISTS (SELECT 1 FROM notification_members m
		                WHERE m.notification_id = notifications.id
		                  AND m.container_id = ? AND m.version_key = ?)
		  LIMIT 1`,
		nodeID, string(model.CardSnoozed), containerID, key,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ListUpdatingNodeNotifications 取某台机器上某个容器「更新中」的通知。
//
// 用它把远端进度落到卡片上：进度事件只带 node_id + container_id，
// 而卡片是照通知 ID 索引的，中间必须靠这一步查出是哪几条消息要改。
// 正常只有一条，但用户可能同时点了网页和卡片，返回列表更稳妥。
//
// 编号之外再按容器名兜一层：容器被重建过时事件里的编号是新的，而库里
// 那条通知可能还记着旧编号（下发时没能对齐）。两条线索任一条能对上就算，
// 免得「更新其实成功了，卡片却永远停在更新中」。
func (s *Store) ListUpdatingNodeNotifications(nodeID int64, containerID, containerName string) ([]model.Notification, error) {
	// 先按成员表认出「哪几条通知提到了这个容器」，再回通知表过滤。
	// 一步 JOIN 也能写，但两张表都有 container_id / container_name，
	// 列名必须逐个加前缀，反而更容易漏。
	ids, err := s.notificationIDsForContainer(containerID, containerName)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	ph := make([]string, len(ids))
	args := make([]any, 0, len(ids)+2)
	args = append(args, nodeID, string(model.CardUpdating))
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	rows, err := s.db.Query(
		`SELECT `+notifCols+` FROM notifications
		  WHERE node_id = ? AND card_state = ? AND id IN (`+strings.Join(ph, ",")+`)
		  ORDER BY id DESC`,
		args...,
	)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ptrs := make([]*model.Notification, len(out))
	for i := range out {
		ptrs[i] = &out[i]
	}
	if err := s.attachMembers(ptrs); err != nil {
		return nil, err
	}
	return out, nil
}

// notificationIDsForContainer 找出提到某个容器的通知。
//
// 编号与名字任一能对上即可：容器被重建过时编号会变，而通知里可能还记着旧的。
func (s *Store) notificationIDsForContainer(containerID, containerName string) ([]int64, error) {
	var (
		rows *sql.Rows
		err  error
	)
	switch {
	case containerID != "" && containerName != "":
		rows, err = s.db.Query(
			`SELECT DISTINCT notification_id FROM notification_members
			  WHERE container_id = ? OR container_name = ?`, containerID, containerName)
	case containerID != "":
		rows, err = s.db.Query(
			`SELECT DISTINCT notification_id FROM notification_members WHERE container_id = ?`, containerID)
	case containerName != "":
		rows, err = s.db.Query(
			`SELECT DISTINCT notification_id FROM notification_members WHERE container_name = ?`, containerName)
	default:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// UpdateNotificationMemberState 记下一个成员容器的更新结果。
func (s *Store) UpdateNotificationMemberState(notificationID int64, containerID, state, errText string) error {
	_, err := s.db.Exec(
		`UPDATE notification_members SET state = ?, error = ?
		  WHERE notification_id = ? AND container_id = ?`,
		state, errText, notificationID, containerID,
	)
	return err
}

// RealignNotificationContainer 把提醒与成员行里的容器编号换成当前实例。
//
// 编号只在容器实例的生命周期里有效：重建一次就变。提醒里存的是发提醒那天的，
// 下发时按名字找回了新编号，就必须写回，否则节点回传结果时对不上这条提醒。
func (s *Store) RealignNotificationContainer(id int64, containerName, newID string) error {
	if id <= 0 || newID == "" {
		return nil
	}
	if _, err := s.db.Exec(`UPDATE notifications SET container_id = ?, updated_at = ? WHERE id = ?`,
		newID, timeToTs(time.Now()), id); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`UPDATE notification_members SET container_id = ? WHERE notification_id = ? AND container_name = ?`,
		newID, id, containerName,
	)
	return err
}

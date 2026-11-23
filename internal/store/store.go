// Package store 提供基于 SQLite 的持久化层。
//
// 选用纯 Go 的 modernc.org/sqlite 驱动，避免 CGO，
// 这样交叉编译 linux/amd64 与 linux/arm64 都不需要目标平台工具链。
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store 数据库句柄。
type Store struct {
	db *sql.DB
}

// Open 打开（必要时创建）数据库并完成建表。
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// SQLite 写操作串行化，单连接可彻底规避 database is locked。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// migrate 建表。全部使用 IF NOT EXISTS，可重复执行。
func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS images (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			reference       TEXT    NOT NULL,
			container_id    TEXT    NOT NULL DEFAULT '',
			container_name  TEXT    NOT NULL DEFAULT '',
			compose_project TEXT    NOT NULL DEFAULT '',
			compose_service TEXT    NOT NULL DEFAULT '',
			compose_deps    TEXT    NOT NULL DEFAULT '',
			origin          TEXT    NOT NULL DEFAULT '',
			image_id        TEXT    NOT NULL DEFAULT '',
			size_bytes      INTEGER NOT NULL DEFAULT 0,
			image_created   INTEGER NOT NULL DEFAULT 0,
			dangling        INTEGER NOT NULL DEFAULT 0,
			local_digest    TEXT    NOT NULL DEFAULT '',
			remote_digest   TEXT    NOT NULL DEFAULT '',
			local_tag       TEXT    NOT NULL DEFAULT '',
			latest_tag      TEXT    NOT NULL DEFAULT '',
			aliases         TEXT    NOT NULL DEFAULT '',
			mode            TEXT    NOT NULL DEFAULT 'auto',
			mode_override   INTEGER NOT NULL DEFAULT 0,
			ignored         INTEGER NOT NULL DEFAULT 0,
			ignored_digest  TEXT    NOT NULL DEFAULT '',
			notified_tag    TEXT    NOT NULL DEFAULT '',
			notified_digest TEXT    NOT NULL DEFAULT '',
			status          TEXT    NOT NULL DEFAULT 'unknown',
			baseline_at     INTEGER NOT NULL DEFAULT 0,
			last_checked_at INTEGER NOT NULL DEFAULT 0,
			last_error      TEXT    NOT NULL DEFAULT '',
			created_at      INTEGER NOT NULL DEFAULT 0,
			updated_at      INTEGER NOT NULL DEFAULT 0,
			UNIQUE(reference, container_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_images_container ON images(container_id)`,

		`CREATE TABLE IF NOT EXISTS notifications (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			image_ref         TEXT    NOT NULL,
			container_id      TEXT    NOT NULL DEFAULT '',
			container_name    TEXT    NOT NULL DEFAULT '',
			node_id           INTEGER NOT NULL DEFAULT 0,
			node_name         TEXT    NOT NULL DEFAULT '',
			version_key       TEXT    NOT NULL DEFAULT '',
			kind              TEXT    NOT NULL,
			title             TEXT    NOT NULL DEFAULT '',
			body              TEXT    NOT NULL DEFAULT '',
			local_summary     TEXT    NOT NULL DEFAULT '',
			remote_summary    TEXT    NOT NULL DEFAULT '',
			local_tag         TEXT    NOT NULL DEFAULT '',
			latest_tag        TEXT    NOT NULL DEFAULT '',
			aliases           TEXT    NOT NULL DEFAULT '',
			read              INTEGER NOT NULL DEFAULT 0,
			feishu_message_id TEXT    NOT NULL DEFAULT '',
			channel           TEXT    NOT NULL DEFAULT 'feishu',
			chat_ref          TEXT    NOT NULL DEFAULT '',
			message_id        TEXT    NOT NULL DEFAULT '',
			card_state        TEXT    NOT NULL DEFAULT 'pending',
			card_detail       TEXT    NOT NULL DEFAULT '',
			compose_project   TEXT    NOT NULL DEFAULT '',
			member_count      INTEGER NOT NULL DEFAULT 1,
			deliveries        TEXT    NOT NULL DEFAULT '',
			created_at        INTEGER NOT NULL DEFAULT 0,
			updated_at        INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_notifications_created ON notifications(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_notifications_ref ON notifications(image_ref, container_id)`,

		// 一条提醒覆盖的容器。单容器提醒也写一行 —— 去重、结果写回、逐容器
		// 结果展示全部只看这张表，代码里不必区分「合并的」和「单条的」。
		`CREATE TABLE IF NOT EXISTS notification_members (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			notification_id INTEGER NOT NULL,
			container_id    TEXT    NOT NULL DEFAULT '',
			container_name  TEXT    NOT NULL DEFAULT '',
			image_ref       TEXT    NOT NULL DEFAULT '',
			version_key     TEXT    NOT NULL DEFAULT '',
			kind            TEXT    NOT NULL DEFAULT '',
			local_tag       TEXT    NOT NULL DEFAULT '',
			latest_tag      TEXT    NOT NULL DEFAULT '',
			local_summary   TEXT    NOT NULL DEFAULT '',
			remote_summary  TEXT    NOT NULL DEFAULT '',
			state           TEXT    NOT NULL DEFAULT 'pending',
			error           TEXT    NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_notification_members_notif ON notification_members(notification_id)`,
		`CREATE INDEX IF NOT EXISTS idx_notification_members_container ON notification_members(notification_id, container_id)`,
		// 按容器反查「这条容器属于哪条提醒」：结果回写与去重都走这条路径。
		`CREATE INDEX IF NOT EXISTS idx_notification_members_cid ON notification_members(container_id)`,
		`CREATE INDEX IF NOT EXISTS idx_notification_members_name ON notification_members(container_name)`,

		`CREATE TABLE IF NOT EXISTS tasks (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			notification_id  INTEGER NOT NULL DEFAULT 0,
			image_ref        TEXT    NOT NULL DEFAULT '',
			container_id     TEXT    NOT NULL DEFAULT '',
			container_name   TEXT    NOT NULL DEFAULT '',
			node_id          INTEGER NOT NULL DEFAULT 0,
			node_name        TEXT    NOT NULL DEFAULT '',
			new_container_id TEXT    NOT NULL DEFAULT '',
			status           TEXT    NOT NULL DEFAULT 'pending',
			triggered_by     TEXT    NOT NULL DEFAULT '',
			log              TEXT    NOT NULL DEFAULT '',
			error            TEXT    NOT NULL DEFAULT '',
			backup_name      TEXT    NOT NULL DEFAULT '',
			started_at       INTEGER NOT NULL DEFAULT 0,
			finished_at      INTEGER NOT NULL DEFAULT 0,
			created_at       INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_created ON tasks(created_at DESC)`,

		`CREATE TABLE IF NOT EXISTS scans (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			started_at  INTEGER NOT NULL DEFAULT 0,
			finished_at INTEGER NOT NULL DEFAULT 0,
			total       INTEGER NOT NULL DEFAULT 0,
			with_update INTEGER NOT NULL DEFAULT 0,
			notified    INTEGER NOT NULL DEFAULT 0,
			forced      INTEGER NOT NULL DEFAULT 0,
			error       TEXT    NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_scans_started ON scans(started_at DESC)`,

		`CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT ''
		)`,

		// 远程节点。token_hash 存 sha256，明文只在生成安装命令时展示一次。
		`CREATE TABLE IF NOT EXISTS nodes (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			name          TEXT    NOT NULL DEFAULT '',
			hub_url       TEXT    NOT NULL DEFAULT '',
			note          TEXT    NOT NULL DEFAULT '',
			token_hash    TEXT    NOT NULL DEFAULT '',
			hostname      TEXT    NOT NULL DEFAULT '',
			os            TEXT    NOT NULL DEFAULT '',
			arch          TEXT    NOT NULL DEFAULT '',
			agent_version TEXT    NOT NULL DEFAULT '',
			protocol      INTEGER NOT NULL DEFAULT 0,
			status        TEXT    NOT NULL DEFAULT 'never',
			last_seen_at  INTEGER NOT NULL DEFAULT 0,
			last_report_at INTEGER NOT NULL DEFAULT 0,
			offline_alert_at INTEGER NOT NULL DEFAULT 0,
			created_at    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_nodes_token ON nodes(token_hash)`,

		// 远端节点的容器快照。每轮上报全量替换，不做增量 diff：
		// 增量合并要处理「容器被删了」「改名字了」「镜像换了」一堆边界，
		// 而一次上报本来就只有几十 KB，全量替换是最不容易出错的做法。
		`CREATE TABLE IF NOT EXISTS node_containers (
			node_id        INTEGER NOT NULL,
			container_id   TEXT    NOT NULL,
			container_name TEXT    NOT NULL DEFAULT '',
			compose_project TEXT   NOT NULL DEFAULT '',
			compose_service TEXT   NOT NULL DEFAULT '',
			compose_deps    TEXT   NOT NULL DEFAULT '',
			image_ref      TEXT    NOT NULL DEFAULT '',
			image_id       TEXT    NOT NULL DEFAULT '',
			local_tag      TEXT    NOT NULL DEFAULT '',
			local_digest   TEXT    NOT NULL DEFAULT '',
			remote_digest  TEXT    NOT NULL DEFAULT '',
			latest_tag     TEXT    NOT NULL DEFAULT '',
			aliases        TEXT    NOT NULL DEFAULT '',
			origin         TEXT    NOT NULL DEFAULT '',
			status         TEXT    NOT NULL DEFAULT '',
			mode           TEXT    NOT NULL DEFAULT '',
			ignored        INTEGER NOT NULL DEFAULT 0,
			last_error     TEXT    NOT NULL DEFAULT '',
			checked_at     INTEGER NOT NULL DEFAULT 0
		)`,
		// 普通索引即可，绝不能建成唯一索引：镜像维度的条目（本机闲置镜像、
		// 纯远端盯梢）容器编号是空串，一条上报里有两个这样的条目就撞唯一约束，
		// 整份快照插入失败、事务回滚 —— 节点页上永远显示 0 个容器。
		`CREATE INDEX IF NOT EXISTS idx_node_containers_node ON node_containers(node_id)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("建表失败: %w", err)
		}
	}
	return s.addMissingColumns()
}

// imageAddedColumns 是 1.1 起为 images 表新增的列。
//
// CREATE TABLE IF NOT EXISTS 对已存在的表一个字段都不会补，
// 而缺列的后果是整个查询在 SELECT 阶段就报 no such column —— 页面直接白掉。
// 所以老库必须逐个 ALTER TABLE 补齐。
var imageAddedColumns = []struct{ name, ddl string }{
	{"origin", "TEXT NOT NULL DEFAULT ''"},
	{"image_id", "TEXT NOT NULL DEFAULT ''"},
	{"size_bytes", "INTEGER NOT NULL DEFAULT 0"},
	{"image_created", "INTEGER NOT NULL DEFAULT 0"},
	{"dangling", "INTEGER NOT NULL DEFAULT 0"},
	// 用户在飞书卡片上「忽略本版本」时记下的远端摘要，见 model.Image.IgnoredDigest。
	{"ignored_digest", "TEXT NOT NULL DEFAULT ''"},
	// 「上一轮提醒过哪一版」。没有这两列时，去重只能靠通知记录本身，
	// 用户清一次已读历史，同一版本就会重新冒出来。
	{"notified_tag", "TEXT NOT NULL DEFAULT ''"},
	{"notified_digest", "TEXT NOT NULL DEFAULT ''"},
	// 容器所属的 compose 项目。老库补上这一列后先为空，下一轮巡检会填上。
	{"compose_project", "TEXT NOT NULL DEFAULT ''"},
	// compose 服务名与 depends_on 列表（后者存 JSON 数组）。
	// 1.11.0 起用于「按依赖顺序重建」：上游先换、下游后换。
	// 老 Agent 不上报，两列留空即可，重建顺序退化成以前的行为。
	{"compose_service", "TEXT NOT NULL DEFAULT ''"},
	{"compose_deps", "TEXT NOT NULL DEFAULT ''"},
}

// notificationAddedColumns 是 1.2.4 起为 notifications 表新增的列。
//
// 加了 Telegram 之后，「把消息 ID 记在哪」不能再叫 feishu_message_id：
// 更新完成后要按渠道去改原来那条消息，而两个渠道的定位方式不一样。
// 老库的 feishu_message_id 由 addMissingColumns 回填到 message_id，不会丢历史。
var notificationAddedColumns = []struct{ name, ddl string }{
	{"channel", "TEXT NOT NULL DEFAULT 'feishu'"},
	{"chat_ref", "TEXT NOT NULL DEFAULT ''"},
	{"message_id", "TEXT NOT NULL DEFAULT ''"},
	// 全量投递记录（JSON 数组）。同时配了两个渠道时一条提醒要各发一份，
	// 只存一个 message_id 就只能改得动其中一个，另一个会一直停在「待更新」。
	{"deliveries", "TEXT NOT NULL DEFAULT ''"},
	// 远端节点上的提醒。node_id 为 0 表示本机，老记录默认就是 0，不用回填。
	{"node_id", "INTEGER NOT NULL DEFAULT 0"},
	{"node_name", "TEXT NOT NULL DEFAULT ''"},
	// 去重键。远端快照每轮整体替换，没有 images.notified_tag 那样的地方
	// 长期记住「这一版说过了」，只能记在通知上。
	{"version_key", "TEXT NOT NULL DEFAULT ''"},
	// 卡片上那句结果说明（失败原因、回滚原因）。只存在于 IM 消息里的话，
	// 网页上就只剩一个「失败」徽章，重启后连消息那边也看不到了。
	{"card_detail", "TEXT NOT NULL DEFAULT ''"},
	// 合并提醒：这条提醒对应哪个 compose 项目、覆盖几个容器。
	// 老记录一律 member_count=1，语义上等同于「单容器提醒」。
	{"compose_project", "TEXT NOT NULL DEFAULT ''"},
	{"member_count", "INTEGER NOT NULL DEFAULT 1"},
}

// taskAddedColumns 是 1.8.2 起为 tasks 表新增的列。
var taskAddedColumns = []struct{ name, ddl string }{
	{"node_id", "INTEGER NOT NULL DEFAULT 0"},
	{"node_name", "TEXT NOT NULL DEFAULT ''"},
}

// addMissingColumns 给老库补上新增的列。
func (s *Store) addMissingColumns() error {
	if err := s.addImageColumns(); err != nil {
		return err
	}
	if err := s.addNodeColumns(); err != nil {
		return err
	}
	if err := s.addNodeContainerColumns(); err != nil {
		return err
	}
	if err := s.addTaskColumns(); err != nil {
		return err
	}
	return s.addNotificationColumns()
}

func (s *Store) addTaskColumns() error {
	have, err := s.tableColumns("tasks")
	if err != nil {
		return err
	}
	for _, col := range taskAddedColumns {
		if have[col.name] {
			continue
		}
		if _, err := s.db.Exec("ALTER TABLE tasks ADD COLUMN " + col.name + " " + col.ddl); err != nil {
			return fmt.Errorf("补列 %s 失败: %w", col.name, err)
		}
	}
	return nil
}

// nodeAddedColumns 是 1.7.5 起为 nodes 表新增的列。
//
// last_report_at 记「最近一次真正收到快照的时间」，和心跳是两个概念：
// 心跳只证明连着，快照才证明这台机器上的容器数据是最新的。页面上
// 「在线但还没收到过快照」要显示成巡检中，而不是一个会误导人的 0。
var nodeAddedColumns = []struct{ name, ddl string }{
	{"last_report_at", "INTEGER NOT NULL DEFAULT 0"},
	// 1.12.0：本次失联发过告警的时刻，0 表示还没报过。
	//
	// 落库而不是只放内存：Hub 重启后同一次失联仍然是同一次，
	// 不能因为「重启了一遍」就再报一遍。
	{"offline_alert_at", "INTEGER NOT NULL DEFAULT 0"},
}

// nodeContainerAddedColumns 是 1.10.0 起为 node_containers 表新增的列。
//
// 注意这里补的是 node_containers 而不是 nodes：容器所属的 compose 项目属于
// 快照里的每一行，不属于节点本身。补错表的表现是老库升级后查询直接报
// no such column: compose_project，节点页的容器列表整块读不出来。
var nodeContainerAddedColumns = []struct{ name, ddl string }{
	{"compose_project", "TEXT NOT NULL DEFAULT ''"},
	// 1.11.0：按 depends_on 排重建顺序要用（compose_deps 存 JSON 数组）。
	{"compose_service", "TEXT NOT NULL DEFAULT ''"},
	{"compose_deps", "TEXT NOT NULL DEFAULT ''"},
}

func (s *Store) addNodeColumns() error {
	have, err := s.tableColumns("nodes")
	if err != nil {
		return err
	}
	for _, col := range nodeAddedColumns {
		if have[col.name] {
			continue
		}
		if _, err := s.db.Exec("ALTER TABLE nodes ADD COLUMN " + col.name + " " + col.ddl); err != nil {
			return fmt.Errorf("补列 %s 失败: %w", col.name, err)
		}
	}
	// 1.7.5 之前 node_containers 上建的是 (node_id, container_id) 唯一索引。
	// 镜像维度那条没有容器编号（空串），一份上报里出现两个就违反约束，
	// 整份快照写不进去 —— 节点页一直显示 0 个容器。这里把旧索引删掉换成普通索引。
	if _, err := s.db.Exec(`DROP INDEX IF EXISTS idx_node_containers`); err != nil {
		return fmt.Errorf("删除旧的快照唯一索引失败: %w", err)
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_node_containers_node ON node_containers(node_id)`); err != nil {
		return fmt.Errorf("重建快照索引失败: %w", err)
	}
	return nil
}

// addNodeContainerColumns 给老库的 node_containers 补列。
func (s *Store) addNodeContainerColumns() error {
	have, err := s.tableColumns("node_containers")
	if err != nil {
		return err
	}
	for _, col := range nodeContainerAddedColumns {
		if have[col.name] {
			continue
		}
		if _, err := s.db.Exec("ALTER TABLE node_containers ADD COLUMN " + col.name + " " + col.ddl); err != nil {
			return fmt.Errorf("补列 %s 失败: %w", col.name, err)
		}
	}
	return nil
}

func (s *Store) addImageColumns() error {
	have, err := s.tableColumns("images")
	if err != nil {
		return err
	}
	needBackfill := !have["origin"]
	// 这两个列是 1.6.1 才有的。只在「这次刚补上」时回填一次：
	// 每次都跑的话，用户点过「恢复提醒」（把标记清空）后重启一次又被填回来，
	// 恢复就成了摆设。
	needNotifyBackfill := !have["notified_tag"]
	for _, col := range imageAddedColumns {
		if have[col.name] {
			continue
		}
		if _, err := s.db.Exec("ALTER TABLE images ADD COLUMN " + col.name + " " + col.ddl); err != nil {
			return fmt.Errorf("补列 %s 失败: %w", col.name, err)
		}
	}
	// 老库的记录没有来源信息，按「有没有容器」回填一次。
	// 少了这一步，升级后原来那些容器监控项会统统跑到「镜像」标签页里去。
	if needBackfill {
		if _, err := s.db.Exec(
			`UPDATE images SET origin = CASE WHEN container_id = '' THEN 'watch' ELSE 'container' END WHERE origin = ''`,
		); err != nil {
			return fmt.Errorf("回填来源失败: %w", err)
		}
	}
	if needNotifyBackfill {
		if err := s.backfillNotified(); err != nil {
			return err
		}
	}
	return nil
}

// backfillNotified 把「已经提醒过哪一版」从旧的痕迹里补出来。
//
// 不回填的后果是：升级后第一轮扫描会认为所有更新都没提醒过，把用户早就看过、
// 甚至点过忽略的版本统统再推一遍。刚说「修好了不再重复」，转头就刷一屏旧提醒。
func (s *Store) backfillNotified() error {
	// 老的「忽略本版本」只记了摘要，直接搬过去。
	if _, err := s.db.Exec(
		`UPDATE images SET notified_digest = ignored_digest WHERE notified_digest = '' AND ignored_digest != ''`,
	); err != nil {
		return fmt.Errorf("回填已提醒摘要失败: %w", err)
	}
	// 更高版本 tag 的那一路只留了通知记录，取最近一条仍有效的。
	// 排除 snoozed：那是用户明确要求「下次再说」，不该被当成已经提醒过。
	if _, err := s.db.Exec(`
		UPDATE images SET notified_tag = COALESCE((
			SELECT n.latest_tag FROM notifications n
			WHERE n.image_ref = images.reference
			  AND n.container_id = images.container_id
			  AND n.latest_tag <> ''
			  AND n.card_state <> 'snoozed'
			ORDER BY n.created_at DESC
			LIMIT 1
		), '') WHERE notified_tag = ''`); err != nil {
		return fmt.Errorf("回填已提醒版本失败: %w", err)
	}
	return nil
}

func (s *Store) addNotificationColumns() error {
	have, err := s.tableColumns("notifications")
	if err != nil {
		return err
	}
	for _, col := range notificationAddedColumns {
		if have[col.name] {
			continue
		}
		if _, err := s.db.Exec("ALTER TABLE notifications ADD COLUMN " + col.name + " " + col.ddl); err != nil {
			return fmt.Errorf("补列 %s 失败: %w", col.name, err)
		}
	}
	// 升级前的通知都只发过飞书，把旧列的值搬到新列，
	// 否则升级后那些还挂着的卡片点按钮能收到回调，但状态改不回去。
	if have["feishu_message_id"] && !have["message_id"] {
		if _, err := s.db.Exec(
			`UPDATE notifications SET channel = 'feishu', message_id = feishu_message_id WHERE message_id = ''`,
		); err != nil {
			return fmt.Errorf("回填消息 ID 失败: %w", err)
		}
	}
	// 老通知没有成员行，补一行出来：去重与结果写回都改成看成员表了，
	// 不回填等于升级后把老历史当成「没提醒过」，同一批版本再推一遍。
	if _, err := s.db.Exec(`
		INSERT INTO notification_members(
			notification_id, container_id, container_name, image_ref, version_key,
			kind, local_tag, latest_tag, local_summary, remote_summary, state)
		SELECT id, container_id, container_name, image_ref, version_key,
			kind, local_tag, latest_tag, local_summary, remote_summary,
			CASE card_state
				WHEN 'done' THEN 'done'
				WHEN 'failed' THEN 'failed'
				WHEN 'rollback' THEN 'failed'
				ELSE 'pending'
			END
		FROM notifications
		WHERE id NOT IN (SELECT notification_id FROM notification_members)`); err != nil {
		return fmt.Errorf("回填通知成员失败: %w", err)
	}
	return nil
}

// tableColumns 读取一张表已有的列名。
func (s *Store) tableColumns(table string) (map[string]bool, error) {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, typ        string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// ---- 时间与布尔值转换（统一用 unix 秒与 0/1 存储，避免驱动差异） ----

func tsToTime(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

func timeToTs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func tsToTimePtr(v int64) *time.Time {
	if v == 0 {
		return nil
	}
	t := time.Unix(v, 0)
	return &t
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// GetSetting 读取单个设置项，不存在时返回空串与 false。
func (s *Store) GetSetting(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetSetting 写入单个设置项。
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}

// AllSettings 返回全部设置项。
func (s *Store) AllSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

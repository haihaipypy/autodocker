package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/haihaipypy/autodocker/internal/model"
)

const taskCols = `id, notification_id, image_ref, container_id, container_name,
	node_id, node_name, new_container_id,
	status, triggered_by, log, error, backup_name, started_at, finished_at, created_at`

func scanTask(sc rowScanner) (model.Task, error) {
	var (
		t          model.Task
		startedAt  int64
		finishedAt int64
		createdAt  int64
	)
	err := sc.Scan(
		&t.ID, &t.NotificationID, &t.ImageRef, &t.ContainerID, &t.ContainerName,
		&t.NodeID, &t.NodeName, &t.NewContainerID,
		&t.Status, &t.TriggeredBy, &t.Log, &t.Error, &t.BackupName,
		&startedAt, &finishedAt, &createdAt,
	)
	if err != nil {
		return t, err
	}
	t.StartedAt = tsToTimePtr(startedAt)
	t.FinishedAt = tsToTimePtr(finishedAt)
	t.CreatedAt = tsToTime(createdAt)
	return t, nil
}

// CreateTask 创建更新任务。
func (s *Store) CreateTask(t *model.Task) (int64, error) {
	now := time.Now()
	if t.Status == "" {
		t.Status = model.TaskPending
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	var id int64
	err := s.db.QueryRow(`
		INSERT INTO tasks(notification_id, image_ref, container_id, container_name,
			node_id, node_name, status, triggered_by, log, error, backup_name, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		RETURNING id`,
		t.NotificationID, t.ImageRef, t.ContainerID, t.ContainerName,
		t.NodeID, t.NodeName,
		string(t.Status), t.TriggeredBy, t.Log, t.Error, t.BackupName, timeToTs(t.CreatedAt),
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	t.ID = id
	return id, nil
}

// GetTask 按主键读取任务。
func (s *Store) GetTask(id int64) (*model.Task, error) {
	t, err := scanTask(s.db.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListTasks 列出更新任务，最新在前。
func (s *Store) ListTasks(limit int) ([]model.Task, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT `+taskCols+` FROM tasks ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// StartTask 把任务置为执行中并记录起始时间与备份容器名。
func (s *Store) StartTask(id int64, backupName string) error {
	now := timeToTs(time.Now())
	_, err := s.db.Exec(`UPDATE tasks SET status = ?, started_at = ?, backup_name = ? WHERE id = ?`,
		string(model.TaskRunning), now, backupName, id)
	return err
}

// AppendTaskLog 追加一行运行日志。
func (s *Store) AppendTaskLog(id int64, line string) error {
	if line == "" {
		return nil
	}
	if len(line) > 4000 {
		line = line[:4000]
	}
	_, err := s.db.Exec(`UPDATE tasks SET log = log || ? WHERE id = ?`, line+"\n", id)
	return err
}

// FinishTask 结束任务并写入终态。
func (s *Store) FinishTask(id int64, status model.TaskStatus, newContainerID, errMsg string) error {
	_, err := s.db.Exec(`UPDATE tasks SET status = ?, new_container_id = ?, error = ?, finished_at = ? WHERE id = ?`,
		string(status), newContainerID, errMsg, timeToTs(time.Now()), id)
	return err
}

// HasRunningTask 判断某容器是否已有进行中的更新任务，防止重复触发。
func (s *Store) HasRunningTask(containerID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE container_id = ? AND status IN (?, ?)`,
		containerID, string(model.TaskPending), string(model.TaskRunning)).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// RecoverStaleTasks 把重启前遗留的「执行中」任务标记为失败，
// 避免重启后状态永远卡在 running。
func (s *Store) RecoverStaleTasks() (int64, error) {
	res, err := s.db.Exec(
		`UPDATE tasks SET status = ?, error = ?, finished_at = ? WHERE status IN (?, ?)`,
		string(model.TaskFailed), "服务重启，任务中断", timeToTs(time.Now()),
		string(model.TaskPending), string(model.TaskRunning),
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

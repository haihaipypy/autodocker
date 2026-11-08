package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/haihaipypy/autodocker/internal/model"
)

// ErrNotFound 记录不存在。
var ErrNotFound = errors.New("记录不存在")

const imageCols = `id, reference, container_id, container_name, origin, image_id, size_bytes,
	image_created, dangling, local_digest, remote_digest,
	local_tag, latest_tag, aliases, mode, mode_override, ignored, ignored_digest, status,
	baseline_at, last_checked_at, last_error, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanImage(sc rowScanner) (model.Image, error) {
	var (
		img                   model.Image
		aliasesJSON           string
		modeOverride, ignored int
		dangling              int
		baselineAt            int64
		lastChecked           int64
		createdAt, updatedAt  int64
	)
	err := sc.Scan(
		&img.ID, &img.Reference, &img.ContainerID, &img.ContainerName,
		&img.Origin, &img.ImageID, &img.SizeBytes, &img.ImageCreated, &dangling,
		&img.LocalDigest, &img.RemoteDigest, &img.LocalTag, &img.LatestTag,
		&aliasesJSON, &img.Mode, &modeOverride, &ignored, &img.IgnoredDigest, &img.Status,
		&baselineAt, &lastChecked, &img.LastError, &createdAt, &updatedAt,
	)
	if err != nil {
		return img, err
	}
	img.ModeOverride = modeOverride != 0
	img.Ignored = ignored != 0
	img.Dangling = dangling != 0
	img.BaselineAt = tsToTime(baselineAt)
	img.LastCheckedAt = tsToTime(lastChecked)
	img.CreatedAt = tsToTime(createdAt)
	img.UpdatedAt = tsToTime(updatedAt)
	if aliasesJSON != "" {
		_ = json.Unmarshal([]byte(aliasesJSON), &img.Aliases)
	}
	if img.Origin == "" {
		// 理论上迁移已经回填过，这里再兜一层，避免旧数据把归属判断漏掉。
		if img.ContainerID == "" {
			img.Origin = model.OriginWatch
		} else {
			img.Origin = model.OriginContainer
		}
	}
	return img, nil
}

// UpsertImage 新增或更新一条监控项。
//
// 冲突时保留用户侧的设定（mode_override / ignored / ignored_digest /
// baseline_at / created_at），只刷新扫描得到的观测值。
func (s *Store) UpsertImage(img *model.Image) (int64, error) {
	now := time.Now()
	if img.CreatedAt.IsZero() {
		img.CreatedAt = now
	}
	img.UpdatedAt = now
	if img.BaselineAt.IsZero() {
		img.BaselineAt = now
	}
	if img.Mode == "" {
		img.Mode = model.ModeAuto
	}
	if img.Origin == "" {
		if img.ContainerID == "" {
			img.Origin = model.OriginWatch
		} else {
			img.Origin = model.OriginContainer
		}
	}
	aliases, err := json.Marshal(img.Aliases)
	if err != nil {
		aliases = []byte("[]")
	}

	var id int64
	err = s.db.QueryRow(`
		INSERT INTO images(
			reference, container_id, container_name, origin, image_id, size_bytes,
			image_created, dangling, local_digest, remote_digest,
			local_tag, latest_tag, aliases, mode, mode_override, ignored, ignored_digest, status,
			baseline_at, last_checked_at, last_error, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(reference, container_id) DO UPDATE SET
			container_name  = excluded.container_name,
			origin          = excluded.origin,
			image_id        = excluded.image_id,
			size_bytes      = excluded.size_bytes,
			image_created   = excluded.image_created,
			dangling        = excluded.dangling,
			local_digest    = excluded.local_digest,
			local_tag       = excluded.local_tag,
			status          = excluded.status,
			last_checked_at = excluded.last_checked_at,
			last_error      = excluded.last_error,
			updated_at      = excluded.updated_at,
			mode            = CASE WHEN images.mode_override = 1 THEN images.mode ELSE excluded.mode END
		RETURNING id`,
		img.Reference, img.ContainerID, img.ContainerName, string(img.Origin), img.ImageID,
		img.SizeBytes, img.ImageCreated, b2i(img.Dangling), img.LocalDigest, img.RemoteDigest,
		img.LocalTag, img.LatestTag, string(aliases), img.Mode, b2i(img.ModeOverride), b2i(img.Ignored),
		img.IgnoredDigest, img.Status,
		timeToTs(img.BaselineAt), timeToTs(img.LastCheckedAt), img.LastError, timeToTs(img.CreatedAt), timeToTs(img.UpdatedAt),
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	img.ID = id

	// 容器维度是「一个容器一条记录」，但唯一键是 (reference, container_id)：
	// 容器的镜像引用一旦变成别的写法，就会插出一条新记录，而旧记录因为
	// container_id 还活着，DeleteImagesMissing 管不到它 —— 表现是「容器」页上
	// 同一个容器出现两行。
	//
	// 真实现场：容器原本跑 `127.0.0.1:5000/myapp:latest`，打完新 tag 之后
	// Docker 对同一个容器报的 Image 变成裸 digest，于是多出一行。
	// notifications / tasks 里的 image_ref、container_id 都是纯文本快照，
	// 没有外键指向 images，所以删掉旧行不会带走历史记录。
	if img.Origin == model.OriginContainer && img.ContainerID != "" {
		if _, derr := s.db.Exec(
			`DELETE FROM images WHERE container_id = ? AND reference <> ?`,
			img.ContainerID, img.Reference,
		); derr != nil {
			return id, fmt.Errorf("清理同容器的旧记录失败: %w", derr)
		}
	}
	return id, nil
}

// GetImage 按主键读取。
func (s *Store) GetImage(id int64) (*model.Image, error) {
	img, err := scanImage(s.db.QueryRow(`SELECT `+imageCols+` FROM images WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &img, nil
}

// FindImage 按「引用 + 容器」定位监控项。
func (s *Store) FindImage(reference, containerID string) (*model.Image, error) {
	img, err := scanImage(s.db.QueryRow(
		`SELECT `+imageCols+` FROM images WHERE reference = ? AND container_id = ?`,
		reference, containerID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &img, nil
}

// FindImageByContainer 按容器 ID 找监控项，不看引用。
//
// 为什么不总是用 (reference, container_id) 精确查：重建到更高版本 tag 之后，
// 这一行的 reference 还是旧的（v3.6.0），而调用方手里拿的是新的（v3.7.0），
// 精确查必然扑空 —— 扑空的表现不是报错，而是「更新成功但页面停在旧版本」。
func (s *Store) FindImageByContainer(containerID string) (*model.Image, error) {
	if containerID == "" {
		return nil, ErrNotFound
	}
	img, err := scanImage(s.db.QueryRow(
		`SELECT `+imageCols+` FROM images WHERE container_id = ? ORDER BY id DESC LIMIT 1`,
		containerID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &img, nil
}

// UpdateImageAfterRebuild 重建成功后，把监控项对齐到新容器与新的镜像引用。
//
// 与「只刷新本地观测值」的写法相比，它同时改 reference 和 image_id：升 tag 的重建里
// 这两项都是变的，只改 local_tag 会让「当前」一栏永远停在旧版本。
func (s *Store) UpdateImageAfterRebuild(id int64, reference, imageID, containerID, containerName, localDigest, localTag string) error {
	_, err := s.db.Exec(`
		UPDATE images SET reference = ?, image_id = ?, container_id = ?, container_name = ?,
			local_digest = ?, local_tag = ?, updated_at = ?
		WHERE id = ?`,
		reference, imageID, containerID, containerName, localDigest, localTag,
		timeToTs(time.Now()), id,
	)
	return err
}

// UpdateImageContainer 把监控项挂到另一个容器编号上，引用不动。
//
// 用途只有一个：库里记的容器号已经失效，但容器本身还在（重建成功却没写回状态、
// 或者容器被删掉后重新建了一个同名的）。容器名是稳定的，编号不是。
// 不修这一列，下一次更新还会拿着旧编号去查，继续 404。
func (s *Store) UpdateImageContainer(id int64, containerID, containerName string) error {
	_, err := s.db.Exec(`
		UPDATE images SET container_id = ?, container_name = ?, updated_at = ?
		WHERE id = ?`,
		containerID, containerName, timeToTs(time.Now()), id,
	)
	return err
}

// ListImages 返回监控项，有更新的排在前面。
//
// 不传 origins 就是全部；传了就只返回这些来源的记录 ——
// 页面上「容器」与「镜像」两个标签页各取所需，互不混淆。
func (s *Store) ListImages(origins ...model.ImageOrigin) ([]model.Image, error) {
	query := `SELECT ` + imageCols + ` FROM images`
	args := make([]any, 0, len(origins))
	if len(origins) > 0 {
		holders := make([]string, 0, len(origins))
		for _, o := range origins {
			holders = append(holders, "?")
			args = append(args, string(o))
		}
		query += ` WHERE origin IN (` + strings.Join(holders, ",") + `)`
	}
	query += ` ORDER BY CASE status
			WHEN 'update_available' THEN 0
			WHEN 'new_version_tag' THEN 1
			WHEN 'unknown' THEN 2
			WHEN 'up_to_date' THEN 3
			ELSE 4 END, container_name ASC, reference ASC`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Image{}
	for rows.Next() {
		img, err := scanImage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

// PruneImages 删除指定来源里、已经不在这份「存活清单」中的记录。
//
// 刻意按来源分开清理：容器项的存活由容器列表决定，纯远端项的存活由 WATCH
// 配置决定，两种判据完全不同，混在一个 DELETE 里会互相误删。
func (s *Store) PruneImages(origin model.ImageOrigin, keepKeys []string) (int64, error) {
	rows, err := s.db.Query(`SELECT id, reference, container_id FROM images WHERE origin = ?`, string(origin))
	if err != nil {
		return 0, err
	}
	type rec struct {
		id  int64
		key string
	}
	var all []rec
	for rows.Next() {
		var (
			id  int64
			ref string
			cid string
		)
		if err := rows.Scan(&id, &ref, &cid); err != nil {
			rows.Close()
			return 0, err
		}
		// 容器项按容器 ID 判断存活，其余按镜像引用判断。
		key := ref
		if origin == model.OriginContainer {
			key = cid
		}
		all = append(all, rec{id: id, key: key})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	keep := make(map[string]struct{}, len(keepKeys))
	for _, k := range keepKeys {
		keep[k] = struct{}{}
	}

	var removed int64
	for _, r := range all {
		if _, ok := keep[r.key]; ok {
			continue
		}
		if _, err := s.db.Exec(`DELETE FROM images WHERE id = ?`, r.id); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// UpdateImageCheck 写入一次扫描的观测结果。
func (s *Store) UpdateImageCheck(id int64, status model.Status, remoteDigest, latestTag string, aliases []string, mode model.DetectMode, errMsg string) error {
	now := time.Now()
	aliasesJSON, err := json.Marshal(aliases)
	if err != nil {
		aliasesJSON = []byte("[]")
	}
	_, err = s.db.Exec(`
		UPDATE images SET
			status = ?, remote_digest = ?, latest_tag = ?, aliases = ?, last_error = ?,
			mode = CASE WHEN mode_override = 1 THEN mode ELSE ? END,
			last_checked_at = ?, updated_at = ?
		WHERE id = ?`,
		string(status), remoteDigest, latestTag, string(aliasesJSON), errMsg,
		string(mode), timeToTs(now), timeToTs(now), id,
	)
	return err
}

// SetImageIgnored 设置忽略标记。
//
// 恢复监控时连 ignored_digest 一起清掉：那个字段记的是「用户已放行的某次远端内容」，
// 用户既然要重新收提醒，就不该再被它压着。
func (s *Store) SetImageIgnored(id int64, ignored bool) error {
	status := model.StatusUnknown
	if ignored {
		status = model.StatusIgnored
	}
	if ignored {
		_, err := s.db.Exec(`UPDATE images SET ignored = ?, status = ?, updated_at = ? WHERE id = ?`,
			b2i(ignored), string(status), timeToTs(time.Now()), id)
		return err
	}
	_, err := s.db.Exec(`UPDATE images SET ignored = ?, ignored_digest = '', status = ?, updated_at = ? WHERE id = ?`,
		b2i(ignored), string(status), timeToTs(time.Now()), id)
	return err
}

// SetImageIgnoredDigest 记下「用户已放行的那次远端内容」。
//
// 只影响 NotifNewVersion 这一种通知：远端摘要不变就不再提醒，
// 摘要一变（仓库又推了新构建）照常通知。传空串等于撤销这一次放行。
func (s *Store) SetImageIgnoredDigest(id int64, digest string) error {
	_, err := s.db.Exec(`UPDATE images SET ignored_digest = ?, updated_at = ? WHERE id = ?`,
		digest, timeToTs(time.Now()), id)
	return err
}

// SetImageMode 设置检测模式覆写。
func (s *Store) SetImageMode(id int64, mode model.DetectMode, override bool) error {
	_, err := s.db.Exec(`UPDATE images SET mode = ?, mode_override = ?, updated_at = ? WHERE id = ?`,
		string(mode), b2i(override), timeToTs(time.Now()), id)
	return err
}

// DeleteImage 删除监控项。
func (s *Store) DeleteImage(id int64) error {
	_, err := s.db.Exec(`DELETE FROM images WHERE id = ?`, id)
	return err
}

// DeleteImageByImageID 删除某个本机镜像对应的镜像记录。
//
// 镜像被删掉之后，原来那条记录就成了指向空气的悬空项，留着只会在页面上
// 显示一个点不动的残影。限定 container_id = ” 是刻意的：容器维度的记录
// 由容器自身的生命周期管，不该被镜像删除顺手带走。
func (s *Store) DeleteImageByImageID(imageID string) error {
	_, err := s.db.Exec(`DELETE FROM images WHERE image_id = ? AND container_id = ''`, imageID)
	return err
}

// DeleteImagesMissing 清理已不存在的容器对应的监控项（纯远端监控项不受影响）。
func (s *Store) DeleteImagesMissing(liveContainerIDs []string) (int64, error) {
	rows, err := s.db.Query(`SELECT id, container_id FROM images WHERE container_id <> ''`)
	if err != nil {
		return 0, err
	}
	type rec struct {
		id  int64
		cid string
	}
	var all []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.cid); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	live := make(map[string]struct{}, len(liveContainerIDs))
	for _, c := range liveContainerIDs {
		live[c] = struct{}{}
	}

	var removed int64
	for _, r := range all {
		if _, ok := live[r.cid]; ok {
			continue
		}
		if _, err := s.db.Exec(`DELETE FROM images WHERE id = ?`, r.id); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// CountImagesByStatus 统计各状态数量。
//
// 不传 origins 就是全部；传了只统计这些来源。首页那五个数字讲的是
// 「被监控的容器」有多少、几个有更新 —— 那是容器维度的事。
// 混进本机的镜像（含没人用的悬空层）会把总数撑得很大，
// 而且和「有更新 / 已是最新」对不上，看着就像统计坏了。
func (s *Store) CountImagesByStatus(origins ...model.ImageOrigin) (map[string]int, error) {
	query := `SELECT status, COUNT(*) FROM images`
	args := make([]any, 0, len(origins))
	if len(origins) > 0 {
		holders := make([]string, 0, len(origins))
		for _, o := range origins {
			holders = append(holders, "?")
			args = append(args, string(o))
		}
		query += ` WHERE origin IN (` + strings.Join(holders, ",") + `)`
	}
	query += ` GROUP BY status`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

package updater

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/haihaipypy/autodocker/internal/composetag"
	"github.com/haihaipypy/autodocker/internal/dockerx"
)

// composeUndoTimeout 撤销改写时用的时限。
//
// 这里故意不接外面那个 ctx：更新失败时 ctx 可能已经被取消，
// 而「把用户的文件恢复原样」必须做完 —— 否则容器回滚了、文件却留着新版本，
// 下一次 compose up 反而把旧容器升上去。
const composeUndoTimeout = 20 * time.Second

// composeProject 一个容器的 compose 身份。
type composeProject struct {
	Service     string
	WorkDir     string
	ConfigFiles []string
}

// composeProjectOf 从容器标签里读出 compose 身份。
func composeProjectOf(insp *dockerx.Inspect) composeProject {
	if insp == nil || insp.Config == nil {
		return composeProject{}
	}
	labels, ok := insp.Config["Labels"].(map[string]any)
	if !ok {
		return composeProject{}
	}
	get := func(key string) string {
		if s, ok := labels[key].(string); ok {
			return s
		}
		return ""
	}
	p := composeProject{
		Service: get("com.docker.compose.service"),
		WorkDir: get("com.docker.compose.project.working_dir"),
	}
	for _, f := range strings.Split(get("com.docker.compose.project.config_files"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			p.ConfigFiles = append(p.ConfigFiles, f)
		}
	}
	return p
}

// projectAccess 读写的抽象：裸机部署直接读盘，容器里部署则借一个临时容器。
type projectAccess interface {
	Read(ctx context.Context, path string) ([]byte, dockerx.FileMeta, error)
	Write(ctx context.Context, path string, data []byte, meta dockerx.FileMeta) error
}

type osAccess struct{}

func (osAccess) Read(_ context.Context, p string) ([]byte, dockerx.FileMeta, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, dockerx.FileMeta{}, err
	}
	buf, err := os.ReadFile(p)
	// 就地覆写不会改属主，所以这里只需要权限位。
	return buf, dockerx.FileMeta{Mode: int64(st.Mode().Perm())}, err
}

func (osAccess) Write(_ context.Context, p string, data []byte, meta dockerx.FileMeta) error {
	return os.WriteFile(p, data, os.FileMode(meta.Mode))
}

type containerAccess struct {
	dk *dockerx.Client
	id string
}

func (a containerAccess) Read(ctx context.Context, p string) ([]byte, dockerx.FileMeta, error) {
	return a.dk.CopyFromContainer(ctx, a.id, p)
}

func (a containerAccess) Write(ctx context.Context, p string, data []byte, meta dockerx.FileMeta) error {
	return a.dk.CopyToContainer(ctx, a.id, path.Dir(p), path.Base(p), data, meta)
}

// composeSession 一次「把 compose 里的版本跟着改」的会话。
//
// 用法固定：拿到之后就 defer Close()，任何一步失败都调 Undo()。
// Undo 必须在 Close 之前 —— 撤销是靠那个临时容器写的。
type composeSession struct {
	access      projectAccess
	cleanup     func()
	composePath string
	composeOld  string
	composeMeta dockerx.FileMeta
	envPath     string
	envOld      string
	envNew      string
	envMeta     dockerx.FileMeta
}

// Close 收拾临时容器。
func (s *composeSession) Close() {
	if s != nil && s.cleanup != nil {
		s.cleanup()
	}
}

// Undo 把文件写回改写前的样子。
func (s *composeSession) Undo() {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), composeUndoTimeout)
	defer cancel()
	if err := s.access.Write(ctx, s.composePath, []byte(s.composeOld), s.composeMeta); err != nil {
		return
	}
	if s.envPath != "" && s.envNew != s.envOld {
		_ = s.access.Write(ctx, s.envPath, []byte(s.envOld), s.envMeta)
	}
}

// syncComposeTag 把 compose 项目里的版本固定点改到这次要更新的版本。
//
// 返回 nil 表示什么都不用做（不是 compose 容器、文件已经是目标版本、或不敢改）；
// 调用方只在更新失败并且回滚时调 Undo()。所有失败都只记一行日志 ——
// 文件改不动不该拦住容器更新本身，但必须让用户知道下次 compose up 会退版本。
func (u *Updater) syncComposeTag(ctx context.Context, insp *dockerx.Inspect, targetRef string, logf func(string, ...any)) *composeSession {
	proj := composeProjectOf(insp)
	if proj.Service == "" || len(proj.ConfigFiles) == 0 {
		return nil
	}

	access, cleanup, err := u.projectAccess(ctx, proj)
	if err != nil {
		logf("注意：读不到 compose 项目文件（%v）；这次只换容器，文件里的版本没跟着改，下次对该项目执行 docker compose up -d 会把版本退回去", err)
		return nil
	}

	current := ""
	if insp.Config != nil {
		if s, ok := insp.Config["Image"].(string); ok {
			current = s
		}
	}
	if current == "" {
		current = insp.Image
	}

	envPath := path.Join(proj.WorkDir, ".env")
	if proj.WorkDir == "" {
		envPath = ""
	}
	var envText string
	envMeta := dockerx.FileMeta{Mode: 0o644}
	if envPath != "" {
		if b, m, rerr := access.Read(ctx, envPath); rerr == nil {
			envText, envMeta = string(b), m
		}
	}

	var lastErr error
	for _, path := range proj.ConfigFiles {
		compose, meta, rerr := access.Read(ctx, path)
		if rerr != nil {
			lastErr = rerr
			continue
		}
		res, perr := composetag.Plan(
			composetag.Files{ComposePath: path, Compose: string(compose), EnvPath: envPath, Env: envText},
			composetag.Request{Service: proj.Service, Current: current, Target: targetRef},
		)
		if perr != nil {
			// 多个 -f 时，某个文件里没有这个服务是正常的，接着试下一个。
			lastErr = perr
			continue
		}
		if !res.Changed {
			logf("%s", res.Note)
			cleanup()
			return nil
		}

		sess := &composeSession{
			access:      access,
			cleanup:     cleanup,
			composePath: path,
			composeOld:  string(compose),
			composeMeta: meta,
			envPath:     envPath,
			envOld:      envText,
			envNew:      res.Env,
			envMeta:     envMeta,
		}
		if err := u.applyCompose(ctx, sess, res, logf); err != nil {
			logf("注意：compose 文件里的版本没跟着改（%v）", err)
			cleanup()
			return nil
		}
		logf("%s", res.Note)
		return sess
	}

	if lastErr != nil {
		logf("注意：compose 文件里的版本没跟着改（%v）", lastErr)
	}
	cleanup()
	return nil
}

// applyCompose 落盘：先备份，再写；写到一半失败就把已写的恢复。
func (u *Updater) applyCompose(ctx context.Context, sess *composeSession, res composetag.Result, logf func(string, ...any)) error {
	if res.Env != sess.envOld && sess.envPath != "" {
		// 备份只是给人看的最后一道保险；写不进去不影响主流程。
		if sess.envOld != "" {
			_ = sess.access.Write(ctx, sess.envPath+".autodocker.bak", []byte(sess.envOld), sess.envMeta)
		}
		if err := sess.access.Write(ctx, sess.envPath, []byte(res.Env), sess.envMeta); err != nil {
			return fmt.Errorf("写 %s 失败：%w", path.Base(sess.envPath), err)
		}
	}
	if res.Compose != sess.composeOld {
		_ = sess.access.Write(ctx, sess.composePath+".autodocker.bak", []byte(sess.composeOld), sess.composeMeta)
		if err := sess.access.Write(ctx, sess.composePath, []byte(res.Compose), sess.composeMeta); err != nil {
			// compose 文件没写成，但 .env 可能已经改了：退回去，别留半套。
			if sess.envPath != "" && res.Env != sess.envOld {
				_ = sess.access.Write(ctx, sess.envPath, []byte(sess.envOld), sess.envMeta)
			}
			return fmt.Errorf("写 %s 失败：%w", path.Base(sess.composePath), err)
		}
	}
	return nil
}

// projectAccess 决定走哪条路读写项目文件。
//
// 裸机部署时进程就在宿主机上，直接读盘最简单；在容器里就得借临时容器 ——
// 这不是可选优化，是唯一可行的路径：compose 文件在宿主机上，
// 而容器内看不到那个目录。
func (u *Updater) projectAccess(ctx context.Context, proj composeProject) (projectAccess, func(), error) {
	if _, err := os.Stat(proj.ConfigFiles[0]); err == nil {
		return osAccess{}, func() {}, nil
	}

	// 先用容器号（进程内 hostname 一般就是短 ID），不行再退回容器名 ——
	// 两者都试一遍，是因为宿主机的 hostname 不一定是容器 ID（有人用 --hostname 改过）。
	var insp *dockerx.Inspect
	var lastErr error
	for _, ref := range []string{u.opt.SelfContainerID, u.opt.SelfContainer} {
		if ref == "" {
			continue
		}
		got, err := u.dk.InspectContainer(ctx, ref)
		if err == nil {
			insp = got
			break
		}
		lastErr = err
	}
	if insp == nil {
		return nil, nil, fmt.Errorf("找不到本服务自身的容器（%s / %s）：%v",
			u.opt.SelfContainerID, u.opt.SelfContainer, lastErr)
	}

	dirs := composeDirs(proj)
	cfg := map[string]any{
		"Image": insp.Image, // 用镜像编号，不依赖某个 tag 还在不在本地
		"Cmd":   []string{"true"},
		// 打上 skip 标签，扫描器就不会把这个临时容器当成「用户要更新的容器」
		// 抓进列表里 —— 它只活几秒，但正好撞上巡检就会在页面上闪一下。
		"Labels": map[string]any{"autodocker.skip": "true"},
	}
	hostCfg := map[string]any{
		"Binds":       dirs,
		"NetworkMode": "none",
	}
	name := fmt.Sprintf("autodocker-composectl-%d", time.Now().UnixNano())
	id, err := u.dk.CreateContainer(ctx, cfg, hostCfg, nil, name)
	if err != nil {
		return nil, nil, fmt.Errorf("借容器读 compose 文件失败：%w", err)
	}
	cleanup := func() {
		cctx, cancel := context.WithTimeout(context.Background(), composeUndoTimeout)
		defer cancel()
		_ = u.dk.RemoveContainer(cctx, id, true, false)
	}
	return containerAccess{dk: u.dk, id: id}, cleanup, nil
}

// composeDirs 需要 bind 进临时容器的目录：compose 文件所在目录与项目目录。
func composeDirs(proj composeProject) []string {
	seen := map[string]bool{}
	var out []string
	add := func(dir string) {
		if dir == "" || dir == "/" || seen[dir] {
			return
		}
		seen[dir] = true
		out = append(out, dir+":"+dir)
	}
	for _, f := range proj.ConfigFiles {
		add(path.Dir(f))
	}
	add(proj.WorkDir)
	return out
}

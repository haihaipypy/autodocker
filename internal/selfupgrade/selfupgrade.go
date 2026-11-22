// Package selfupgrade 让 Agent 把「把自己换成新版本」这件事做完。
//
// 为什么不能直接换：Agent 的二进制是宿主机上的文件绑进容器的，进程自己改不了
// 那个文件（挂载是只读的）；就算能改，跑着的还是旧进程，而停掉自己之后
// 没人负责把新容器拉起来。
//
// 所以分两步：
//  1. Agent 只做准备工作 —— 从 Hub 下载新二进制、把当前容器的完整配置记成一份
//     现场快照、再拉起一个一次性的 helper 容器；
//  2. helper 动刀 —— 停旧容器、换二进制、按原配置建新的、观察能否稳定运行，
//     任何一步失败就整套回滚（旧容器重新起来、旧二进制放回去）。
//
// helper 跑的是**新**二进制自己的 `upgrade-helper` 子命令，不是旧的那份：
// 旧二进制里根本没有这段逻辑。由此带来一个无法绕过的限制 —— 这一版之前装上的
// Agent 不认识 upgrade 指令，第一次仍然要在目标机上重跑一次安装命令；
// 从那以后每次都能自动更换。
package selfupgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/haihaipypy/autodocker/internal/dockerx"
)

const (
	// HelperCommand helper 子命令名，main 在加载配置之前就分流到这里。
	HelperCommand = "upgrade-helper"

	// helperContainer helper 的固定容器名。用固定名字的好处是：上一次升级
	// 半途失败留下了残骸，下一次能按名字清掉，不会越积越多。
	helperContainer = "autodocker-agent-upgrade"

	// DataDir Agent 容器的数据目录。安装脚本固定把数据卷挂在这里，
	// 升级现场（新二进制、配置快照、结果文件）全放它下面 —— 这些必须能跨
	// 容器重建活下来。
	DataDir = "/data"

	specPath   = DataDir + "/upgrade.json"
	resultPath = DataDir + "/upgrade-result.json"
	nextBinary = DataDir + "/autodocker.next"

	// 二进制进容器的落点，见安装脚本：-v $BINDIR/autodocker:/app/autodocker:ro
	binDestination = "/app/autodocker"
	helperBinDir   = "/host-bin"

	// stableWindow 新容器连续跑满这么久才算升级成功。
	//
	// 太短会把「启动即崩、但 restart 策略又把它拉起来」误判成成功；
	// 太长则让用户多等。8 秒足够覆盖配置错误、端口冲突这类立刻暴露的问题。
	stableWindow = 8 * time.Second
)

// AuxContainers 自升级过程会短暂（甚至长期）活在宿主机上的容器名。
//
// 巡检必须把它们排除掉：helper 在「新 Agent 已经起来、旧容器还没删」的那十几秒里
// 是完全合法的存在，Agent 一启动就巡检，很容易正好撞上，于是 Hub 的节点列表里
// 冒出一个用户从没装过的 autodocker-agent-old，还带着「可选更新」的标记。
// 就算它最后被删掉了，那一轮上报已经存进库，要等下一轮全量替换才消失；
// 要是删除失败（磁盘、权限、Docker 抽风），它就会一直挂在那里被人点「更新」。
func AuxContainers(self string) []string {
	self = strings.TrimPrefix(strings.TrimSpace(self), "/")
	out := []string{helperContainer}
	if self != "" {
		out = append(out, self+"-old")
	}
	return out
}

// 两种 Agent 安装方式。
const (
	ModeBinary = "binary" // Hub 下发二进制，挂进 alpine 里跑
	ModeImage  = "image"  // 直接跑镜像（跨架构节点只能这样）
)

// Spec 交给 helper 的现场快照。
//
// 刻意存**原始配置**而不是「重建步骤」：helper 只需要照着 Docker 自己报告过的
// 配置再建一个同样的容器，中间不掺任何判断，出错的面最小。
type Spec struct {
	Container string         `json:"container"` // 容器名（不含前导斜杠）
	Mode      string         `json:"mode"`
	Image     string         `json:"image"` // 新容器用的镜像引用
	Config    map[string]any `json:"config"`
	HostCfg   map[string]any `json:"host_config"`
	SockBind  string         `json:"sock_bind"`
	DataBind  string         `json:"data_bind"`
	BinBind   string         `json:"bin_bind,omitempty"` // binary 模式：宿主机二进制目录
	BinName   string         `json:"bin_name,omitempty"` // binary 模式：二进制文件名
	FromVer   string         `json:"from_version"`
	TargetVer string         `json:"target_version"`
	At        int64          `json:"at"`
}

// Result 一次升级的结局，写在数据卷里给新 Agent 启动时上报。
//
// 为什么要落盘而不是直接回传 Hub：helper 成功时旧进程已经被停掉了，
// 失败时回来的也是旧进程 —— 两条路都没有一条活着的连接可以回传。
type Result struct {
	OK         bool   `json:"ok"`
	From       string `json:"from,omitempty"`
	Target     string `json:"target"`
	Error      string `json:"error,omitempty"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`
}

// Options Agent 侧准备升级所需的输入。
type Options struct {
	HubURL        string
	Token         string
	SelfContainer string // 自身容器名，来自环境变量
	SelfID        string // 自身容器编号，可能为空
	CurrentVer    string
	TargetVer     string // Hub 的版本
	ImageRef      string // 镜像模式下要拉的目标镜像
}

// Prepare 做好升级前的全部准备，拉起 helper。
//
// 返回的字符串是给用户看的一句话（会作为指令回执显示在页面上）。
// 函数返回时 helper 已经起来了，旧进程还能活几秒 —— 正好把这句话送出去。
func Prepare(ctx context.Context, dk *dockerx.Client, opt Options, log *slog.Logger) (string, error) {
	insp, err := findSelf(ctx, dk, opt)
	if err != nil {
		return "", err
	}
	if insp.State == nil || !insp.State.Running {
		return "", errors.New("自身容器不在运行，无法自动升级")
	}
	name := strings.TrimPrefix(insp.Name, "/")
	if name == "" {
		name = opt.SelfContainer
	}

	spec := Spec{
		Container: name,
		Config:    replayableConfig(insp.Config),
		HostCfg:   replayableHostConfig(insp.HostConfig),
		FromVer:   opt.CurrentVer,
		TargetVer: opt.TargetVer,
		At:        time.Now().Unix(),
	}

	for _, m := range insp.Mounts {
		switch m.Destination {
		case "/var/run/docker.sock":
			spec.SockBind = m.Source + ":/var/run/docker.sock"
		case DataDir:
			// 命名卷用卷名，绑定目录用宿主机路径 —— 两种写法 Docker 都认。
			if m.Type == "volume" {
				spec.DataBind = m.Name + ":" + DataDir
			} else {
				spec.DataBind = m.Source + ":" + DataDir
			}
		case binDestination:
			if m.Type == "bind" && m.Source != "" {
				spec.Mode = ModeBinary
				spec.BinBind = filepath.Dir(m.Source) + ":" + helperBinDir
				spec.BinName = filepath.Base(m.Source)
			}
		}
	}
	if spec.SockBind == "" {
		return "", errors.New("自身容器没有挂载 docker.sock，无法自动升级")
	}
	if spec.DataBind == "" {
		return "", errors.New("自身容器没有挂载 " + DataDir + " 数据卷，无法自动升级")
	}
	if spec.Mode == "" {
		// 没有那块二进制挂载就是镜像方式装的：新版本从镜像来。
		spec.Mode = ModeImage
		image, _ := insp.Config["Image"].(string)
		if ref := strings.TrimSpace(opt.ImageRef); ref != "" {
			image = ref
		}
		if image == "" {
			return "", errors.New("读不到自身容器使用的镜像引用，无法自动升级")
		}
		spec.Image = image
		// 镜像方式换的是"跑哪个镜像"，所以重建配置里的 Image 也要跟着换。
		// 二进制方式恰好相反：镜像还是 alpine，换的是挂进去的那个文件。
		spec.Config["Image"] = image
	}

	// 清掉上一轮的残骸与结果：结果文件留着会让新 Agent 把旧结局当成这次的。
	//
	// 先看 helper 是不是还活着 —— 活着说明上一次升级正在动刀，
	// 这时候再去动同一台机器等于两个 helper 抢同一个容器。
	if h, herr := dk.InspectContainer(ctx, helperContainer); herr == nil {
		if h.State != nil && h.State.Running {
			return "", errors.New("这台机器上已有升级在进行，等它结束再试")
		}
		_ = dk.RemoveContainer(ctx, helperContainer, true, false)
	}
	// 上一轮帮手中途挂掉可能留下改过名的旧容器，按名字清掉。
	//
	// 只清「已经停了的」：如果它还在跑，说明它才是正在服役的那一份
	// （上一次换到一半就断了，用户手工把它拉了起来），删掉等于把节点删了。
	if old, oerr := dk.InspectContainer(ctx, name+"-old"); oerr == nil {
		if old.State == nil || !old.State.Running {
			_ = dk.RemoveContainer(ctx, name+"-old", true, false)
		}
	}
	_ = os.Remove(resultPath)

	if spec.Mode == ModeBinary {
		// alpine 里没有 curl，但宿主机的 shell 有 —— 下载这件事由 Agent 自己做，
		// helper 只负责把文件搬到位置。少一个外部依赖，就少一处可能失败的地方。
		if err := downloadBinary(ctx, opt.HubURL, opt.Token, nextBinary); err != nil {
			return "", err
		}
	} else {
		log.Info("拉取新镜像", "image", spec.Image)
		pullCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		err := dk.PullImage(pullCtx, spec.Image, func(format string, args ...any) {
			log.Info(fmt.Sprintf(format, args...), "stage", "pull")
		})
		cancel()
		if err != nil {
			return "", fmt.Errorf("拉取新镜像失败: %w", err)
		}
	}

	buf, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", err
	}
	// 先写临时文件再改名：helper 读到半个 JSON 会直接退出，而那时旧容器还没被动过。
	if err := writeFileAtomic(specPath, buf, 0o600); err != nil {
		return "", fmt.Errorf("写入升级现场失败: %w", err)
	}

	helperCfg := map[string]any{
		"Image":  spec.Image,
		"Labels": map[string]string{"autodocker.role": "upgrade-helper"},
		"Cmd":    []string{HelperCommand, "--spec", specPath},
	}
	if spec.Mode == ModeBinary {
		// 二进制方式：helper 就是新二进制自己，所以显式覆盖 ENTRYPOINT；
		// 它躺在数据卷里（rw），正好可以被这个容器执行。
		helperCfg["Image"] = selfImage(insp)
		helperCfg["Entrypoint"] = []string{nextBinary}
	}
	binds := []string{spec.SockBind, spec.DataBind}
	if spec.BinBind != "" {
		binds = append(binds, spec.BinBind)
	}
	helperHostCfg := map[string]any{
		"Binds":         binds,
		"RestartPolicy": map[string]any{"Name": "no"},
		// helper 一退出就被回收：它是耗材，留着只会让下一次的「按名清理」有活干。
		"AutoRemove": true,
	}

	log.Info("启动升级 helper", "name", helperContainer, "mode", spec.Mode)
	id, err := dk.CreateContainer(ctx, helperCfg, helperHostCfg, nil, helperContainer)
	if err != nil {
		return "", fmt.Errorf("创建升级 helper 失败: %w", err)
	}
	if err := dk.StartContainer(ctx, id); err != nil {
		_ = dk.RemoveContainer(ctx, id, true, false)
		return "", fmt.Errorf("启动升级 helper 失败: %w", err)
	}

	return fmt.Sprintf("已开始升级到 %s，节点会重启一次，约 10 秒后自动回来", opt.TargetVer), nil
}

// RunHelper helper 进程的全部逻辑：一套「要么换成新版、要么原样退回」的动作。
//
// 它是被 Docker 单独拉起来的，和旧进程没有父子关系 —— 所以旧容器被停掉、
// 二进制被替换，都不会打断这里的执行。这是整件事能成立的关键。
func RunHelper(args []string) error {
	path := parseSpecArg(args)
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	var spec Spec
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取升级现场失败: %w", err)
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return fmt.Errorf("解析升级现场失败: %w", err)
	}
	log.Info("开始升级", "container", spec.Container, "mode", spec.Mode,
		"from", spec.FromVer, "to", spec.TargetVer)

	// 给旧进程留出把回执与日志送出去的时间。它马上就要被停掉了，
	// 用户看到的「已开始升级」就是这几秒里发出去的。
	time.Sleep(2 * time.Second)

	res := Result{From: spec.FromVer, Target: spec.TargetVer, StartedAt: time.Now().Unix()}
	dk, derr := dockerx.New("")
	if derr != nil {
		res.Error = derr.Error()
		writeResult(res)
		return derr
	}
	defer dk.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	err = upgrade(ctx, dk, spec, log)
	res.OK = err == nil
	if err != nil {
		res.Error = err.Error()
		log.Error("升级失败，已回滚", "err", err)
	} else {
		log.Info("升级完成", "version", spec.TargetVer)
	}
	res.FinishedAt = time.Now().Unix()
	writeResult(res)
	return err
}

// upgrade 执行替换；失败时负责把现场恢复成升级前的样子。
func upgrade(ctx context.Context, dk *dockerx.Client, spec Spec, log *slog.Logger) error {
	oldName := spec.Container + "-old"

	// 先停旧的。不停直接建同名的会撞名字；就算改了名字，两个 Agent 也会
	// 抢同一条节点连接，Hub 那边看到一个节点疯狂上下线。
	log.Info("停止旧 Agent", "container", spec.Container)
	if err := dk.StopContainer(ctx, spec.Container, 10); err != nil {
		return fmt.Errorf("停止旧 Agent 失败: %w", err)
	}
	if err := dk.RenameContainer(ctx, spec.Container, oldName); err != nil {
		_ = dk.StartContainer(ctx, spec.Container)
		return fmt.Errorf("重命名旧 Agent 失败: %w", err)
	}

	// 回滚现场：旧容器改名后还在（只是停了），把名字换回来再启动即可。
	restore := func() {
		if err := dk.RenameContainer(ctx, oldName, spec.Container); err != nil {
			log.Error("回滚：恢复容器名失败", "err", err)
			return
		}
		if err := dk.StartContainer(ctx, spec.Container); err != nil {
			log.Error("回滚：启动旧 Agent 失败", "err", err)
		}
	}

	if spec.Mode == ModeBinary {
		if err := installBinary(spec); err != nil {
			restore()
			return fmt.Errorf("替换二进制失败: %w", err)
		}
	}

	log.Info("创建新 Agent", "image", spec.Image)
	newID, err := dk.CreateContainer(ctx, spec.Config, spec.HostCfg, nil, spec.Container)
	if err != nil {
		restoreBinary(spec)
		restore()
		return fmt.Errorf("创建新 Agent 失败: %w", err)
	}
	if err := dk.StartContainer(ctx, newID); err != nil {
		_ = dk.RemoveContainer(ctx, newID, true, false)
		restoreBinary(spec)
		restore()
		return fmt.Errorf("启动新 Agent 失败: %w", err)
	}

	if msg, ok := waitStable(ctx, dk, newID); !ok {
		log.Error("新 Agent 没站住，开始回滚", "reason", msg)
		_ = dk.RemoveContainer(ctx, newID, true, false)
		restoreBinary(spec)
		restore()
		return errors.New(msg)
	}

	// 新版跑稳了才删旧容器：顺序反过来的话，新版起不来就两头都没有了。
	//
	// 删失败不影响这次升级（新版已经在跑），但那个改名后的容器会一直躺在
	// 宿主机上：巡检会把它当成用户的容器上报，Hub 节点列表里就多出一个
	// 「autodocker-agent-old」。所以这里重试几次，仍失败至少留下明确的日志。
	log.Info("删除旧 Agent 容器", "container", oldName)
	for i := 0; i < 3; i++ {
		err := dk.RemoveContainer(ctx, oldName, true, false)
		if err == nil {
			return nil
		}
		if i == 2 {
			log.Error("旧 Agent 容器删不掉，它可能继续出现在监控列表里",
				"container", oldName, "err", err)
			break
		}
		time.Sleep(time.Second)
	}
	return nil
}

// waitStable 盯一会儿新容器，确认它不是在崩溃重启的循环里。
func waitStable(ctx context.Context, dk *dockerx.Client, id string) (string, bool) {
	deadline := time.Now().Add(stableWindow + 7*time.Second)
	var okSince time.Time
	for {
		running, _, status, err := dk.State(ctx, id)
		if err != nil {
			return "读取新容器状态失败: " + err.Error(), false
		}
		if !running {
			return "新 Agent 启动后没有保持运行（状态：" + status + "），已回滚", false
		}
		if okSince.IsZero() {
			okSince = time.Now()
		} else if time.Since(okSince) >= stableWindow {
			return "", true
		}
		if time.Now().After(deadline) {
			return "新 Agent 未能在预期时间内稳定运行，已回滚", false
		}
		time.Sleep(time.Second)
	}
}

// installBinary 把数据卷里的新二进制搬到宿主机目录。
//
// 顺序：先把新文件拷成 .new（同目录，保证 rename 是原子的），旧文件改名成 .prev
// 留作回滚，最后 .new 顶上。任何一步失败都不会出现「目标文件不存在且没有备份」。
func installBinary(spec Spec) error {
	dst := filepath.Join(helperBinDir, spec.BinName)
	tmp := dst + ".new"
	prev := dst + ".prev"

	if err := copyFile(nextBinary, tmp, 0o755); err != nil {
		return err
	}
	_ = os.Remove(prev)
	if err := os.Rename(dst, prev); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Rename(prev, dst)
		return err
	}
	return nil
}

func restoreBinary(spec Spec) {
	if spec.Mode != ModeBinary || spec.BinName == "" {
		return
	}
	dst := filepath.Join(helperBinDir, spec.BinName)
	prev := dst + ".prev"
	if _, err := os.Stat(prev); err != nil {
		return // 没有备份说明还没走到替换那一步
	}
	_ = os.Remove(dst)
	_ = os.Rename(prev, dst)
}

// findSelf 在容器列表里找自己。优先用编号，其次是几个已知的容器名。
//
// 为什么要试多个名字：SELF_CONTAINER 的默认值是 "autodocker"（给本机部署用的），
// 而安装脚本装出来的 Agent 叫做 "autodocker-agent"。容器内通常能靠 hostname
// 拿到自己的短编号，那条路最准；拿不到时按名字挨个试一遍，比直接报错有用。
func findSelf(ctx context.Context, dk *dockerx.Client, opt Options) (*dockerx.Inspect, error) {
	if id := strings.TrimSpace(opt.SelfID); id != "" {
		if insp, err := dk.InspectContainer(ctx, id); err == nil {
			return insp, nil
		}
	}
	candidates := []string{opt.SelfContainer, "autodocker-agent", "autodocker"}
	for _, name := range candidates {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if insp, err := dk.InspectContainer(ctx, name); err == nil {
			return insp, nil
		}
	}
	// 都扑空时再按名字扫一遍列表：InspectContainer 对带斜杠/别名的名字比较挑，
	// 而列表里的 Name 是 Docker 自己归一化过的。
	list, err := dk.ListContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("列举容器失败: %w", err)
	}
	for _, c := range list {
		for _, name := range candidates {
			if name != "" && c.Name == name {
				return dk.InspectContainer(ctx, c.ID)
			}
		}
	}
	return nil, errors.New("找不到 Agent 自身的容器，无法自动升级（容器被改名了？）")
}

// replayableConfig 只保留重建时需要的 Config 字段。
//
// 全量照搬会把 Hostname、Image 的本地编号这类「上一次运行时的产物」一起带进来：
// Hostname 重复会让新容器跟旧容器撞名，Image 编号则可能指向一个即将被删的镜像。
func replayableConfig(in map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"Image", "Env", "Entrypoint", "Cmd", "Labels",
		"User", "WorkingDir", "StopSignal", "StopTimeout"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	return out
}

// replayableHostConfig 只保留重建时需要的 HostConfig 字段。
//
// 白名单而不是黑名单：Docker 对未知的 HostConfig 字段会直接报错，而黑名单
// 每遇到一个新版本新增的运行时字段就得补一次，迟早漏。
func replayableHostConfig(in map[string]any) map[string]any {
	keys := []string{
		"Binds", "RestartPolicy", "NetworkMode", "LogConfig", "Privileged",
		"CapAdd", "CapDrop", "SecurityOpt", "Memory", "NanoCpus", "PidsLimit",
		"Devices", "Tmpfs", "ExtraHosts", "Dns", "DnsOptions", "DnsSearch",
		"Init", "ShmSize", "Ulimits", "CgroupnsMode", "ReadonlyRootfs",
		"IpcMode", "PidMode", "UTSMode", "UsernsMode", "OomScoreAdj", "Sysctls",
	}
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	return out
}

func selfImage(insp *dockerx.Inspect) string {
	img, _ := insp.Config["Image"].(string)
	return img
}

// downloadBinary 从 Hub 取与自己同一份的可执行文件。
func downloadBinary(ctx context.Context, hubURL, token, dst string) error {
	base := strings.TrimRight(strings.TrimSpace(hubURL), "/")
	if base == "" {
		return errors.New("没有 Hub 地址，无法下载新版本")
	}
	url := fmt.Sprintf("%s/api/agent/binary?arch=%s", base, runtime.GOARCH)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Agent-Token", token)

	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("从 Hub 下载新版本失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("从 Hub 下载新版本失败：HTTP %d", resp.StatusCode)
	}

	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	cerr := f.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("下载中断: %w", err)
	}
	if cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	// 半个二进制是这里最危险的失败形态：装上去之后容器起不来，
	// 而报错跟「下载失败」毫无关系。宁可在这里拒绝，也不要换上去。
	if n < 1<<20 {
		_ = os.Remove(tmp)
		return fmt.Errorf("下载到的文件只有 %d 字节，明显不完整，已放弃升级", n)
	}
	magic := make([]byte, 4)
	if rf, err := os.Open(tmp); err == nil {
		_, _ = io.ReadFull(rf, magic)
		rf.Close()
	}
	if string(magic) != "\x7fELF" {
		_ = os.Remove(tmp)
		return errors.New("下载到的不是 Linux 可执行文件，已放弃升级")
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Chmod(dst, mode)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeResult(res Result) {
	buf, err := json.Marshal(res)
	if err != nil {
		return
	}
	_ = writeFileAtomic(resultPath, buf, 0o600)
}

// ConsumeResult 取走上一次升级的结局（没有则返回 nil），取完即删。
//
// 删掉是必要的：结果只该被上报一次，留着会让之后每次重启都以为
// 刚刚又升级了一次。
func ConsumeResult() *Result {
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		return nil
	}
	_ = os.Remove(resultPath)
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil
	}
	return &res
}

// Cleanup 清掉上一次升级可能留下的 helper 残骸。
//
// helper 正常结束会被 Docker 自动回收（AutoRemove），这里兜的是它被强杀、
// 或者宿主机重启打断的情况 —— 那种残骸占着名字，下一次升级会撞名。
func Cleanup(ctx context.Context, dk *dockerx.Client, log *slog.Logger) {
	if err := dk.RemoveContainer(ctx, helperContainer, true, false); err == nil {
		log.Info("已清理上次升级留下的 helper 容器")
	}
}

func parseSpecArg(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--spec=") {
			return strings.TrimPrefix(a, "--spec=")
		}
		if a == "--spec" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return specPath
}

package updater

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/haihaipypy/autodocker/internal/dockerx"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// 从容器标签里读 compose 身份：服务名、项目目录、配置文件。
func TestComposeProjectOf(t *testing.T) {
	insp := &dockerx.Inspect{Config: map[string]any{
		"Image": "chaitin/safeline-mgt:9.4.3",
		"Labels": map[string]any{
			"com.docker.compose.service":              "mgt",
			"com.docker.compose.project.working_dir":  "/data/safeline",
			"com.docker.compose.project.config_files": "/data/safeline/docker-compose.yaml,/data/safeline/override.yaml",
		},
	}}
	p := composeProjectOf(insp)
	if p.Service != "mgt" || p.WorkDir != "/data/safeline" {
		t.Fatalf("身份读错了: %+v", p)
	}
	if len(p.ConfigFiles) != 2 || p.ConfigFiles[1] != "/data/safeline/override.yaml" {
		t.Fatalf("配置文件列表读错了: %v", p.ConfigFiles)
	}

	// 非 compose 容器：全空，调用方据此直接跳过。
	plain := &dockerx.Inspect{Config: map[string]any{"Image": "nginx:1.25"}}
	if p := composeProjectOf(plain); p.Service != "" || len(p.ConfigFiles) != 0 {
		t.Fatalf("普通容器不该被认成 compose: %+v", p)
	}
}

// bind 进去的目录要带上挂载写法，并且去重、不挂根目录。
func TestComposeDirs(t *testing.T) {
	p := composeProject{
		WorkDir:     "/data/safeline",
		ConfigFiles: []string{"/data/safeline/docker-compose.yaml", "/etc/other/x.yaml", "/x.yaml"},
	}
	dirs := composeDirs(p)
	want := []string{"/data/safeline:/data/safeline", "/etc/other:/etc/other"}
	if len(dirs) != len(want) {
		t.Fatalf("dirs = %v，期望 %v", dirs, want)
	}
	for i := range want {
		if dirs[i] != want[i] {
			t.Errorf("dirs[%d] = %s，期望 %s", i, dirs[i], want[i])
		}
	}
}

// 整条链路：读文件 → 判定 → 改 .env → 失败时能原样退回去。
func TestSyncComposeTagRewritesEnvAndUndo(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "docker-compose.yaml")
	envPath := filepath.Join(dir, ".env")

	compose := "services:\n  mgt:\n    image: ${IMAGE_PREFIX}/safeline-mgt:${IMAGE_TAG}\n"
	env := "IMAGE_PREFIX=chaitin\nIMAGE_TAG=9.4.3\n"
	if err := os.WriteFile(composePath, []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}

	u := &Updater{opt: Options{SelfContainer: "autodocker"}, log: discardLogger()}
	insp := &dockerx.Inspect{Config: map[string]any{
		"Image": "chaitin/safeline-mgt:9.4.3",
		"Labels": map[string]any{
			"com.docker.compose.service":              "mgt",
			"com.docker.compose.project.working_dir":  dir,
			"com.docker.compose.project.config_files": composePath,
		},
	}}
	var lines []string
	sess := u.syncComposeTag(context.Background(), insp, "chaitin/safeline-mgt:9.4.8",
		func(f string, a ...any) { lines = append(lines, f) })
	if sess == nil {
		t.Fatalf("应当返回会话，日志：%v", lines)
	}
	defer sess.Close()

	got, _ := os.ReadFile(envPath)
	if !strings.Contains(string(got), "IMAGE_TAG=9.4.8") {
		t.Fatalf(".env 没改成新版本：\n%s", got)
	}
	// 权限位要保住：.env 常常是 0600。Windows 上根本没有这个位，跳过。
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(envPath); st.Mode().Perm() != 0o600 {
			t.Errorf("权限位被改了: %o", st.Mode().Perm())
		}
	}
	if _, err := os.Stat(envPath + ".autodocker.bak"); err != nil {
		t.Errorf("没留备份文件: %v", err)
	}

	sess.Undo()
	got, _ = os.ReadFile(envPath)
	if string(got) != env {
		t.Errorf("撤销后没恢复原样：\n%s", got)
	}
}

// 文件与容器对不上时必须一个字节都不动 —— 这是最要紧的一条。
func TestSyncComposeTagLeavesFilesAloneOnMismatch(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "docker-compose.yaml")
	envPath := filepath.Join(dir, ".env")
	compose := "services:\n  mgt:\n    image: chaitin/safeline-mgt:9.4.3\n"
	env := "IMAGE_TAG=9.4.3\n"
	_ = os.WriteFile(composePath, []byte(compose), 0o644)
	_ = os.WriteFile(envPath, []byte(env), 0o600)

	u := &Updater{opt: Options{SelfContainer: "autodocker"}, log: discardLogger()}
	insp := &dockerx.Inspect{Config: map[string]any{
		// 容器跑的是 9.4.7，文件写的是 9.4.3
		"Image": "chaitin/safeline-mgt:9.4.7",
		"Labels": map[string]any{
			"com.docker.compose.service":              "mgt",
			"com.docker.compose.project.working_dir":  dir,
			"com.docker.compose.project.config_files": composePath,
		},
	}}
	var lines []string
	sess := u.syncComposeTag(context.Background(), insp, "chaitin/safeline-mgt:9.4.8",
		func(f string, a ...any) { lines = append(lines, f) })
	if sess != nil {
		sess.Close()
		t.Fatal("对不上时不该返回会话")
	}
	if got, _ := os.ReadFile(composePath); string(got) != compose {
		t.Error("compose 文件被动了")
	}
	if got, _ := os.ReadFile(envPath); string(got) != env {
		t.Error(".env 被动了")
	}
	if len(lines) == 0 || !strings.Contains(lines[0], "没跟着改") {
		t.Errorf("应当留一条警告，实际：%v", lines)
	}
}

// 普通容器（没有 compose 标签）：直接跳过，不建临时容器、不读文件。
func TestSyncComposeTagSkipsPlainContainer(t *testing.T) {
	u := &Updater{opt: Options{SelfContainer: "autodocker"}, log: discardLogger()}
	insp := &dockerx.Inspect{Config: map[string]any{"Image": "nginx:1.25.3"}}
	if sess := u.syncComposeTag(context.Background(), insp, "nginx:1.25.4", func(string, ...any) {}); sess != nil {
		sess.Close()
		t.Fatal("普通容器不该有会话")
	}
}

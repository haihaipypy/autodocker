package composetag

import (
	"strings"
	"testing"
)

// 现场最常见的形态：tag 由 .env 里的变量决定，动的是 .env 那一行。
func TestPlanRewritesEnvVar(t *testing.T) {
	compose := `services:
  pg:
    image: ${POSTGRES_IMAGE:?POSTGRES_IMAGE required}
  mgt:
    image: ${IMAGE_PREFIX}/safeline-mgt${REGION}${ARCH_SUFFIX}:${IMAGE_TAG:?image tag required}
    depends_on:
      - pg
`
	env := "IMAGE_PREFIX=chaitin\nREGION=\nARCH_SUFFIX=\nIMAGE_TAG=9.4.3\n"

	res, err := Plan(
		Files{ComposePath: "/data/safeline/docker-compose.yaml", Compose: compose, EnvPath: "/data/safeline/.env", Env: env},
		Request{Service: "mgt", Current: "chaitin/safeline-mgt:9.4.3", Target: "chaitin/safeline-mgt:9.4.8"},
	)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !res.Changed {
		t.Fatal("应当判定为需要改写")
	}
	if strings.Contains(res.Env, "IMAGE_TAG=9.4.3") || !strings.Contains(res.Env, "IMAGE_TAG=9.4.8") {
		t.Fatalf(".env 没改对：\n%s", res.Env)
	}
	if res.Compose != compose {
		t.Error("tag 是变量时，compose 文件不该被动过")
	}
	if !strings.Contains(res.Note, "IMAGE_TAG") || !strings.Contains(res.Note, "9.4.8") {
		t.Errorf("日志没写清改了哪里：%s", res.Note)
	}
}

// 字面量 tag：只改 image 那一行，其余逐字保留。
func TestPlanRewritesLiteralTag(t *testing.T) {
	compose := `services:
  web:
    image: "nginx:1.25.3"   # 反代
    ports:
      - "8080:80"
  db:
    image: postgres:16.3
`
	res, err := Plan(
		Files{ComposePath: "/srv/app/docker-compose.yml", Compose: compose},
		Request{Service: "web", Current: "nginx:1.25.3", Target: "nginx:1.25.4"},
	)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !res.Changed {
		t.Fatal("应当判定为需要改写")
	}
	if !strings.Contains(res.Compose, `"nginx:1.25.4"   # 反代`) {
		t.Errorf("image 行没改对：\n%s", res.Compose)
	}
	if !strings.Contains(res.Compose, "postgres:16.3") {
		t.Error("别的服务被误改了")
	}
}

// 文件里已经指向目标版本：不重复改，也不要报错 —— 批量更新同一项目的第二个服务时会走到这里。
func TestPlanAlreadyOnTarget(t *testing.T) {
	files := Files{
		ComposePath: "/data/safeline/docker-compose.yaml",
		Compose:     "services:\n  mgt:\n    image: chaitin/safeline-mgt:9.4.8\n",
	}
	res, err := Plan(files, Request{Service: "mgt", Current: "chaitin/safeline-mgt:9.4.3", Target: "chaitin/safeline-mgt:9.4.8"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if res.Changed {
		t.Fatal("已经是目标版本，不该再改")
	}
	if !strings.Contains(res.Note, "不用改") {
		t.Errorf("提示没说清：%s", res.Note)
	}
}

// 文件与容器对不上就一个字节都不动：说明这份文件管不到这个容器。
func TestPlanRefusesWhenFileDisagreesWithContainer(t *testing.T) {
	files := Files{
		ComposePath: "/data/safeline/docker-compose.yaml",
		Compose:     "services:\n  mgt:\n    image: chaitin/safeline-mgt:9.4.3\n",
	}
	_, err := Plan(files, Request{Service: "mgt", Current: "chaitin/safeline-mgt:9.4.7", Target: "chaitin/safeline-mgt:9.4.8"})
	if err == nil {
		t.Fatal("应当拒绝改写")
	}
	for _, want := range []string{"9.4.3", "9.4.7", "对不上"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("原因里缺少 %q：%v", want, err)
		}
	}
}

// 仓库对不上：绝不能把 nginx 改到 redis 去。
func TestPlanRefusesOtherRepository(t *testing.T) {
	files := Files{
		ComposePath: "/srv/app/docker-compose.yml",
		Compose:     "services:\n  web:\n    image: nginx:1.25.3\n",
	}
	_, err := Plan(files, Request{Service: "web", Current: "nginx:1.25.3", Target: "redis:7.4"})
	if err == nil || !strings.Contains(err.Error(), "不是同一个仓库") {
		t.Fatalf("应当拒绝：%v", err)
	}
}

// digest 固定的镜像没有 tag 可改。
func TestPlanRefusesDigestPin(t *testing.T) {
	files := Files{
		ComposePath: "/srv/app/docker-compose.yml",
		Compose:     "services:\n  web:\n    image: nginx@sha256:aaaa\n",
	}
	_, err := Plan(files, Request{Service: "web", Current: "nginx:1.25.3", Target: "nginx:1.25.4"})
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("应当拒绝：%v", err)
	}
}

// 变量在 .env 里根本没有：不许猜，直接拒绝。
func TestPlanRefusesMissingEnvVar(t *testing.T) {
	files := Files{
		ComposePath: "/data/app/docker-compose.yaml",
		Compose:     "services:\n  web:\n    image: nginx:${NGINX_TAG}\n",
		EnvPath:     "/data/app/.env",
		Env:         "OTHER=1\n",
	}
	_, err := Plan(files, Request{Service: "web", Current: "nginx:1.25.3", Target: "nginx:1.25.4"})
	if err == nil || !strings.Contains(err.Error(), "NGINX_TAG") {
		t.Fatalf("应当拒绝并点出变量名：%v", err)
	}
}

// 没有 .env 却说 tag 是变量：拒绝，并说清缺什么。
func TestPlanRefusesVarWithoutEnvFile(t *testing.T) {
	files := Files{
		ComposePath: "/data/app/docker-compose.yaml",
		Compose:     "services:\n  web:\n    image: nginx:${NGINX_TAG}\n",
	}
	_, err := Plan(files, Request{Service: "web", Current: "nginx:1.25.3", Target: "nginx:1.25.4"})
	if err == nil || !strings.Contains(err.Error(), ".env") {
		t.Fatalf("应当拒绝：%v", err)
	}
}

// image 没写 tag（隐含 latest）：没有可改的地方。
func TestPlanRefusesImageWithoutTag(t *testing.T) {
	files := Files{
		ComposePath: "/srv/app/docker-compose.yml",
		Compose:     "services:\n  web:\n    image: nginx\n",
	}
	_, err := Plan(files, Request{Service: "web", Current: "nginx:latest", Target: "nginx:1.25.4"})
	if err == nil || !strings.Contains(err.Error(), "没写 tag") {
		t.Fatalf("应当拒绝：%v", err)
	}
}

// 服务名找不到：报清楚是哪个服务，调用方会拿它去试下一个配置文件。
func TestPlanServiceNotFound(t *testing.T) {
	files := Files{
		ComposePath: "/srv/app/docker-compose.yml",
		Compose:     "services:\n  web:\n    image: nginx:1.25.3\n",
	}
	_, err := Plan(files, Request{Service: "detect", Current: "nginx:1.25.3", Target: "nginx:1.25.4"})
	if err == nil || !strings.Contains(err.Error(), "detect") {
		t.Fatalf("应当报找不到服务：%v", err)
	}
}

// .env 里原来带引号，改完还得带引号 —— 否则某些字段（含空格）会解析出问题。
func TestSetEnvValueKeepsQuotes(t *testing.T) {
	out, ok := setEnvValue("A=1\nIMAGE_TAG=\"9.4.3\"\nB=2\n", "IMAGE_TAG", "9.4.8")
	if !ok {
		t.Fatal("没找到这一行")
	}
	if !strings.Contains(out, `IMAGE_TAG="9.4.8"`) {
		t.Errorf("引号没保住：\n%s", out)
	}
	if !strings.Contains(out, "A=1") || !strings.Contains(out, "B=2") {
		t.Error("别的行被动了")
	}
}

// 行尾注释不是注释分隔符以外的内容：YAML 只认「空格 + #」。
func TestStripInlineComment(t *testing.T) {
	cases := map[string]string{
		"nginx:1.25.3 # 反代": "nginx:1.25.3",
		"myrepo/app#1":      "myrepo/app#1",
		"nginx:1.25.3":      "nginx:1.25.3",
	}
	for in, want := range cases {
		if got := stripInlineComment(in); got != want {
			t.Errorf("stripInlineComment(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// compose 的插值有四种写法，都得认。
func TestInterpolate(t *testing.T) {
	env := map[string]string{"A": "1", "EMPTY": ""}
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"x${A}y", "x1y", true},
		{"${MISSING:-d}", "d", true},
		{"${MISSING}", "", false},
		{"${EMPTY:-d}", "d", true},
		{"${A:?must}", "1", true},
		{"plain", "plain", true},
	}
	for _, c := range cases {
		got, ok := expand(c.in, env)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("expand(%q) = %q,%v，期望 %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// 容器已经是目标版本、文件还停在旧的：这是「上次更新成功但文件没跟上」，
// 补上正好 —— 不然下一次 compose up 又把容器退回去，这一条就是为了还这笔账。
func TestPlanSyncsWhenContainerAlreadyOnTarget(t *testing.T) {
	files := Files{
		ComposePath: "/data/safeline/docker-compose.yaml",
		Compose:     "services:\n  mgt:\n    image: chaitin/safeline-mgt:9.4.3\n",
	}
	res, err := Plan(files, Request{Service: "mgt", Current: "chaitin/safeline-mgt:9.4.8", Target: "chaitin/safeline-mgt:9.4.8"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !res.Changed || !strings.Contains(res.Compose, "chaitin/safeline-mgt:9.4.8") {
		t.Fatalf("应当把文件补到 9.4.8：changed=%v\n%s", res.Changed, res.Compose)
	}
}

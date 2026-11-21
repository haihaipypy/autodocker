package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 用户填的地址决定了安装命令能不能用。这里只校验形态，不校验连通性 ——
// 但形态错了（少了协议头、带了端口没写协议）必须当场拦住，
// 否则生成出一条跑不起来的命令，用户只会以为「装完了但 Hub 里没有」。
func TestValidateHubURL(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"", false},
		{"   ", false},
		{"192.168.2.10:19999", false},       // 少了协议头
		{"hub.1day.vip", false},             // 同上
		{"http://192.168.2.10:19999", true}, // 局域网
		{"https://hub.1day.vip", true},      // 公网 + TLS
		{"https://hub.1day.vip/fnos", true}, // 反代在子路径下
		{"ftp://hub.1day.vip", false},       // 协议不对
		{"http://", false},                  // 没有主机
		{"https://hub.1day.vip?x=1", false}, // 带查询参数
		{"https://hub.1day.vip#a", false},   // 带锚点
		{"ws://hub.1day.vip", false},        // Agent 侧自己会换 ws，不归用户填
	}
	for _, c := range cases {
		got := validateHubURL(c.in)
		if c.ok && got != "" {
			t.Errorf("%q 应通过，却报错: %s", c.in, got)
		}
		if !c.ok && got == "" {
			t.Errorf("%q 应被拒绝，却通过了", c.in)
		}
	}
}

// 安装脚本是用户直接 `curl | sh` 的东西，出一点问题都是灾难性的：
// 引号没转好会把地址截断、少一个 set -eu 会把半截失败当成功。
func TestScriptTemplate(t *testing.T) {
	sh := scriptTemplate("http://192.168.2.10:19999", "haihaipypy/autodocker:latest")

	required := []string{
		"#!/bin/sh",                        // 必须是 POSIX sh，群晖/飞牛/OpenWrt 都能跑
		"set -eu",                          // 出错就停，别带着半截状态继续
		"--token=",                         // 令牌从参数读，脚本里不留明文
		"docker.sock:/var/run/docker.sock", // 不给 socket 就看不到任何容器
		"autodocker-agent-data",            // 数据卷要命名，升级时才能保住令牌
		"restart=unless-stopped",           // 开机自启靠它
		"AUTODOCKER_MODE=agent",            // 角色切换
		"http://192.168.2.10:19999",        // Hub 地址
		"haihaipypy/autodocker:latest",
	}
	for _, want := range required {
		if !strings.Contains(sh, want) {
			t.Errorf("脚本缺少关键片段 %q", want)
		}
	}

	// 脚本里不能出现「占位符没替换」的痕迹
	if strings.Contains(sh, "__HUB_URL__") || strings.Contains(sh, "%s") {
		t.Error("脚本里残留了未替换的占位符")
	}
	// 没有令牌时必须报错退出，不能静默装一个连不上的 Agent
	if !strings.Contains(sh, "缺少节点令牌") {
		t.Error("脚本缺少空令牌的守卫")
	}
}

// 地址里带单引号时不能把 shell 引号闭合掉，否则后面的内容会被当成命令执行。
//
// 判断方式不是查字符串——转义后的内容里照样含有 `; rm`，肉眼看着像没转，
// 实际它在单引号内部。真正可靠的是让 shell 自己解析一遍：引号没配对就报语法错。
func TestScriptTemplateIsValidShell(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("当前环境没有 sh，跳过语法校验")
	}

	for _, hub := range []string{
		"http://192.168.2.10:19999",
		"https://hub.example.com'; rm -rf /tmp/x; echo '",
		"https://hub.example.com/$(whoami)/`id`",
	} {
		script := scriptTemplate(hub, "haihaipypy/autodocker:latest")
		f := filepath.Join(t.TempDir(), "install.sh")
		if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
			t.Fatalf("写临时脚本失败: %v", err)
		}
		out, err := exec.Command(shPath, "-n", f).CombinedOutput()
		if err != nil {
			t.Fatalf("地址 %q 生成的脚本语法不通过: %v\n%s\n---\n%s", hub, err, out, script)
		}
	}
}

// 转义后的形式必须和 shQuote 的输出一致，防止哪天有人把 shQuote 摘掉。
func TestScriptTemplateUsesQuotedURL(t *testing.T) {
	hub := "https://hub.example.com"
	script := scriptTemplate(hub, "img:latest")
	if !strings.Contains(script, "HUB_URL='"+hub+"'") {
		t.Errorf("Hub 地址没有被单引号包裹:\n%s", script)
	}
}

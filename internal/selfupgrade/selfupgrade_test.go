package selfupgrade

import "testing"

// 重建配置里的 Image 必须来自「这一轮要用的镜像」，不能带上一次运行时的产物：
// Hostname 撞名、Image 指向一个即将被删的本地编号，都会让新容器起不来。
func TestReplayableConfigKeepsOnlyRebuildFields(t *testing.T) {
	in := map[string]any{
		"Image":      "alpine:3.24",
		"Env":        []any{"AUTODOCKER_MODE=agent"},
		"Entrypoint": []any{"/app/autodocker"},
		"Hostname":   "0d5c3f2a1b9e",
		"Domainname": "",
		"Tty":        false,
	}
	out := replayableConfig(in)
	if out["Image"] != "alpine:3.24" {
		t.Errorf("Image 应保留: %v", out)
	}
	for _, k := range []string{"Env", "Entrypoint"} {
		if _, ok := out[k]; !ok {
			t.Errorf("%s 应保留", k)
		}
	}
	if _, ok := out["Hostname"]; ok {
		t.Error("Hostname 是上一次运行的产物，不该带进新容器")
	}

	hostIn := map[string]any{
		"Binds":         []any{"/var/run/docker.sock:/var/run/docker.sock"},
		"NetworkMode":   "bridge",
		"RestartPolicy": map[string]any{"Name": "unless-stopped"},
		// 下面这些是运行时状态，Docker 的 create 接口不接受，必须滤掉
		"Runtime":         "runc",
		"ConsoleSize":     []any{0, 0},
		"CgroupParent":    "",
		"CgroupnsMode":    "private",
		"AutoRemove":      false,
		"NetworkSettings": map[string]any{},
	}
	hostOut := replayableHostConfig(hostIn)
	if hostOut["NetworkMode"] != "bridge" {
		t.Errorf("NetworkMode 应保留: %v", hostOut)
	}
	for _, k := range []string{"Runtime", "ConsoleSize", "NetworkSettings"} {
		if _, ok := hostOut[k]; ok {
			t.Errorf("%s 是运行时字段，不该出现在重建配置里", k)
		}
	}
}

func TestParseSpecArg(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--spec", "/tmp/a.json"}, "/tmp/a.json"},
		{[]string{"--spec=/tmp/b.json"}, "/tmp/b.json"},
		{[]string{}, specPath},
	}
	for _, c := range cases {
		if got := parseSpecArg(c.args); got != c.want {
			t.Errorf("parseSpecArg(%v) = %q，期望 %q", c.args, got, c.want)
		}
	}
}

// 巡检要排掉自升级过程里的容器。这两个名字拼错一个，Hub 上就会多出一条
// 用户从没装过的「autodocker-agent-old」，还标着「可选更新」。
func TestAuxContainers(t *testing.T) {
	got := AuxContainers("autodocker-agent")
	want := []string{"autodocker-agent-upgrade", "autodocker-agent-old"}
	if len(got) != len(want) {
		t.Fatalf("应返回 %d 个名字，实际 %v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个应为 %q，实际 %q", i, want[i], got[i])
		}
	}

	// 容器名被改过时，-old 要跟着改；否则排除列表对不上，旧容器照样被上报
	if g := AuxContainers("/my-agent"); len(g) != 2 || g[1] != "my-agent-old" {
		t.Errorf("带前导斜杠的名字没处理对: %v", g)
	}
	// 没配名字时只剩 helper，不能拼出一个光秃秃的 "-old"
	if g := AuxContainers(""); len(g) != 1 || g[0] != "autodocker-agent-upgrade" {
		t.Errorf("空名字时不该拼出 -old: %v", g)
	}
}

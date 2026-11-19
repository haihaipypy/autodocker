package dingtalk

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/haihaipypy/autodocker/internal/model"
)

func sampleNotification() *model.Notification {
	return &model.Notification{
		ID:            7,
		ImageRef:      "nginx:1.25",
		ContainerName: "web",
		Kind:          model.NotifNewTag,
		Title:         "web 有新版本可用",
		Body:          "当前版本 `1.25`，仓库中已存在更高版本 **1.27**。",
		LocalTag:      "1.25",
		LatestTag:     "1.27",
	}
}

// 三个变量名是代码与卡片模板之间的硬约定，改了模板就对不上，
// 卡片发出去是一片空白 —— 这条把名字钉死。
func TestUpdateParamsKeys(t *testing.T) {
	p := BuildUpdateParams(sampleNotification())
	if p[VarTitle] == "" || p[VarStatus] == "" || p[VarBody] == "" {
		t.Fatalf("三个模板变量都必须有值: %+v", p)
	}
	if p[VarStatus] != "待更新" {
		t.Fatalf("状态 = %q，期望「待更新」", p[VarStatus])
	}
	if !strings.Contains(p[VarBody], "nginx:1.25") || !strings.Contains(p[VarBody], "1.27") {
		t.Fatalf("正文要能看出镜像和可用版本，实际: %s", p[VarBody])
	}
}

func TestStateParams(t *testing.T) {
	n := sampleNotification()
	cases := []struct {
		state  model.CardState
		status string
	}{
		{model.CardUpdating, "正在更新"},
		{model.CardDone, "更新完成"},
		{model.CardFailed, "更新失败"},
		{model.CardRollback, "已回滚"},
		{model.CardIgnored, "已忽略"},
		{model.CardSnoozed, "已稍后"},
	}
	for _, c := range cases {
		p := BuildStateParams(n, c.state, "细节")
		if p[VarStatus] != c.status {
			t.Errorf("状态 %s 的 status = %q，期望 %q", c.state, p[VarStatus], c.status)
		}
		if !strings.Contains(p[VarBody], "细节") {
			t.Errorf("状态 %s 的正文丢了 detail: %s", c.state, p[VarBody])
		}
	}
}

// 钉钉对 cardParamMap 单个值限 1KB，超了整个请求报 param.invalid，
// 看不出是哪个变量太长。截断必须做，且不能切出半个汉字。
func TestClipKeepsUTF8Valid(t *testing.T) {
	long := strings.Repeat("镜像更新提醒", 200) // 远超 900 字节
	got := clip(long, 900)
	if len(got) > 900 {
		t.Fatalf("截断后仍有 %d 字节，上限 900", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("截断把多字节字符切坏了，钉钉会判非法参数")
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatal("截断后应带省略号，否则用户以为内容就这么多")
	}
	// 短内容不该被动
	if clip("短", 900) != "短" {
		t.Fatal("没超限的内容不该被改")
	}
}

func TestTestParamsHasButtonHint(t *testing.T) {
	p := BuildTestParams("cid123")
	if !strings.Contains(p[VarBody], "任意一个按钮") {
		t.Fatalf("测试卡片必须提示用户点按钮，否则只验了「发得出去」: %s", p[VarBody])
	}
	done := BuildTestDoneParams("2026-10-29 11:00:00")
	if done[VarStatus] != "回调正常" {
		t.Fatalf("回执状态 = %q", done[VarStatus])
	}
}

// 外部推送的卡片数据：长正文必须截断。
//
// 钉钉对 cardParamMap 单个值限制 1KB，超了整个请求被判「param.invalid」，
// 报错里不说是哪个变量太长 —— 用户只会看到一条发不出去的消息。
func TestBuildPushParamsClipsLongBody(t *testing.T) {
	long := strings.Repeat("这是一段很长的消息内容。", 200)
	p := BuildPushParams("备份完成", long)
	if len(p[VarBody]) > 900 {
		t.Fatalf("正文没截到 900 字节以内，得到 %d", len(p[VarBody]))
	}
	if !utf8.ValidString(p[VarBody]) {
		t.Fatal("截断把多字节字符切坏了")
	}
	if p[VarStatus] != "通知" {
		t.Fatalf("状态位应固定为「通知」，得到 %q", p[VarStatus])
	}
	if p[VarTitle] != "备份完成" {
		t.Fatalf("标题被改动了：%q", p[VarTitle])
	}

	// 标题空着时给个兜底：卡片上那格空着比填满更难看。
	if got := BuildPushParams("", "正文")[VarTitle]; got == "" {
		t.Fatal("标题为空时应补一个兜底文案")
	}
}

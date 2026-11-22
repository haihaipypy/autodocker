package hubserver

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/haihaipypy/autodocker/internal/hubproto"
)

// 版本比较只认「两边都能解析成版本号」的情况。
//
// 开发机构建的 "dev" 与正式版比大小没有意义，硬比会把每个节点
// 在开发时反复重启一遍。
func TestNeedUpgrade(t *testing.T) {
	cases := []struct {
		agent, hub string
		want       bool
	}{
		{"1.8.3", "1.8.4", true},
		{"1.8.4", "1.8.4", false},
		{"1.8.5", "1.8.4", false},
		{"v1.7.5", "1.8.4", true},
		{"1.8.3", "1.10.0", true},
		{"1.10.0", "1.8.4", false},
		{"", "1.8.4", false},
		{"dev", "1.8.4", false},
		{"1.8.4", "dev", false},
	}
	for _, c := range cases {
		if got := needUpgrade(c.agent, c.hub); got != c.want {
			t.Errorf("needUpgrade(%q, %q) = %v，期望 %v", c.agent, c.hub, got, c.want)
		}
	}
}

// 同一个目标版本失败两次之后就该停手：再试只会一遍遍把节点重启掉。
func TestAutoBlockedStopsRetrying(t *testing.T) {
	s := New(Config{Version: "1.8.4"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if s.autoBlocked(1, "1.8.4") {
		t.Fatal("没试过的时候不该被拦")
	}
	s.noteAutoAttempt(1, "1.8.4")
	if !s.autoBlocked(1, "1.8.4") {
		t.Error("刚试过，冷却期内应被拦")
	}
	// 冷却期过了可以再试一次
	s.upMu.Lock()
	rec := s.autoTries[1]
	rec.at = time.Now().Add(-2 * autoRetryCooldown)
	s.autoTries[1] = rec
	s.upMu.Unlock()
	if s.autoBlocked(1, "1.8.4") {
		t.Error("冷却期过后应该允许再试")
	}

	s.noteAutoAttempt(1, "1.8.4") // 第二次
	if !s.autoBlocked(1, "1.8.4") {
		t.Error("试满两次后应长期放弃")
	}
	// 目标版本变了（Hub 又升级了）要重新给机会
	if s.autoBlocked(1, "1.8.5") {
		t.Error("换了目标版本应重新允许")
	}
}

// 升级结局回来时要把「正在升级」摘掉，失败也一样 ——
// 挂着那条状态会让按钮永远是灰的。
func TestNoteUpgradeResultClearsState(t *testing.T) {
	s := New(Config{Version: "1.8.4"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.setUpgrading(7, upgradeState{From: "1.8.2", Target: "1.8.4", Started: time.Now()})
	if !s.IsUpgrading(7) {
		t.Fatal("应先处于升级中")
	}
	s.noteUpgradeResult(7, "nas", &hubproto.UpgradeResult{OK: false, Target: "1.8.4", Error: "端口冲突"})
	if s.IsUpgrading(7) {
		t.Error("失败后也应摘掉升级状态")
	}
	last := s.LastUpgrades()[7]
	if last.OK || last.Error != "端口冲突" {
		t.Errorf("失败原因应保留给页面: %+v", last)
	}
}

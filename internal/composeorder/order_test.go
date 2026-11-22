package composeorder

import (
	"reflect"
	"testing"
)

func names(ns []Node) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Name)
	}
	return out
}

// 典型依赖链：反代、后台、数据库。快照里顺序是乱的，排完必须从库到反代。
func TestSortPutsUpstreamFirst(t *testing.T) {
	in := []Node{
		{Name: "safeline-tengine", Service: "safeline-tengine", Deps: []string{"safeline-mgt"}},
		{Name: "safeline-detector", Service: "safeline-detector"},
		{Name: "safeline-mgt", Service: "safeline-mgt", Deps: []string{"safeline-pg"}},
		{Name: "safeline-pg", Service: "safeline-pg"},
	}
	got := names(Sort(in))
	want := []string{"safeline-detector", "safeline-pg", "safeline-mgt", "safeline-tengine"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("顺序不对：\n got %v\nwant %v", got, want)
	}
}

// 没有依赖时必须原样返回：一次更新把无关容器重排，用户会以为别处也动了。
func TestSortKeepsOrderWithoutDeps(t *testing.T) {
	in := []Node{
		{Name: "c", Service: "c"},
		{Name: "a", Service: "a"},
		{Name: "b", Service: "b"},
	}
	if got := names(Sort(in)); !reflect.DeepEqual(got, []string{"c", "a", "b"}) {
		t.Fatalf("无依赖时不该重排，实际 %v", got)
	}
}

// 老 Agent 不上报 service（全空）→ 一律视为无依赖。
func TestSortWithoutServiceKeepsOrder(t *testing.T) {
	in := []Node{{Name: "x", Deps: []string{"y"}}, {Name: "y"}, {Name: "z"}}
	if got := names(Sort(in)); !reflect.DeepEqual(got, []string{"x", "y", "z"}) {
		t.Fatalf("拿不到 service 时不该猜，实际 %v", got)
	}
}

// 只更新项目里的一部分：依赖的目标不在本批，算已满足，不能因此排不动。
func TestSortIgnoresDepsOutsideBatch(t *testing.T) {
	in := []Node{
		{Name: "web", Service: "web", Deps: []string{"db"}},
		{Name: "worker", Service: "worker", Deps: []string{"db", "redis"}},
	}
	if got := names(Sort(in)); !reflect.DeepEqual(got, []string{"web", "worker"}) {
		t.Fatalf("外部依赖不该影响顺序，实际 %v", got)
	}
}

// 自引用与环：不能卡死，也不能丢容器。
func TestSortSurvivesCycles(t *testing.T) {
	in := []Node{
		{Name: "a", Service: "a", Deps: []string{"b"}},
		{Name: "b", Service: "b", Deps: []string{"a"}},
		{Name: "c", Service: "c", Deps: []string{"c"}},
		{Name: "d", Service: "d"},
	}
	out := Sort(in)
	if len(out) != len(in) {
		t.Fatalf("节点丢了：%v", names(out))
	}
	// c 的自引用被忽略，入度为 0，先落位；a、b 成环按原顺序接在后面。
	if got := names(out); !reflect.DeepEqual(got, []string{"c", "d", "a", "b"}) {
		t.Fatalf("成环的按原顺序接在后面即可，实际 %v", got)
	}
}

// 排序不能改动调用方传进来的切片。
func TestSortDoesNotMutateInput(t *testing.T) {
	in := []Node{
		{Name: "b", Service: "b", Deps: []string{"a"}},
		{Name: "a", Service: "a"},
	}
	_ = Sort(in)
	if in[0].Name != "b" {
		t.Fatalf("入参被改动了：%v", names(in))
	}
}

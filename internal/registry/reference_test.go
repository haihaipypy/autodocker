package registry

import "testing"

func TestParseReference(t *testing.T) {
	cases := []struct {
		in       string
		registry string
		repo     string
		tag      string
	}{
		{"nginx:1.25", "registry-1.docker.io", "library/nginx", "1.25"},
		{"nginx", "registry-1.docker.io", "library/nginx", "latest"},
		{"joplin/server:2.13.5", "registry-1.docker.io", "joplin/server", "2.13.5"},
		{"ghcr.io/foo/bar:1.2.3", "ghcr.io", "foo/bar", "1.2.3"},
		{"myregistry:5000/app:1.0", "myregistry:5000", "app", "1.0"},
		{"myregistry:5000/app", "myregistry:5000", "app", "latest"},
		{"localhost/foo:dev", "localhost", "foo", "dev"},
		// 摘要固定：只保留仓库与 tag，忽略 @sha256:...
		{"nginx:1.25@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			"registry-1.docker.io", "library/nginx", "1.25"},
	}
	for _, c := range cases {
		got, err := ParseReference(c.in)
		if err != nil {
			t.Errorf("ParseReference(%q) 报错: %v", c.in, err)
			continue
		}
		if got.Registry != c.registry || got.Repository != c.repo || got.Tag != c.tag {
			t.Errorf("ParseReference(%q) = %s|%s|%s, 期望 %s|%s|%s",
				c.in, got.Registry, got.Repository, got.Tag, c.registry, c.repo, c.tag)
		}
	}
}

func TestParseReferenceErrors(t *testing.T) {
	for _, in := range []string{"", "   ", "@sha256:abc", "/"} {
		if _, err := ParseReference(in); err == nil {
			t.Errorf("ParseReference(%q) 应当报错", in)
		}
	}
}

// WithTag 是「发现更高版本 tag」场景的落点：必须真的把 tag 换成新版本号。
func TestWithTag(t *testing.T) {
	cases := []struct {
		ref  string
		tag  string
		want string
	}{
		// 保持原有书写形式，不冒 library/ 出来
		{"nginx:1.25", "1.27", "nginx:1.27"},
		{"alpine", "3.21", "alpine:3.21"},
		{"joplin/server:2.13.5", "2.13.6", "joplin/server:2.13.6"},
		{"ghcr.io/foo/bar:1.2.3", "1.3.0", "ghcr.io/foo/bar:1.3.0"},
		// 带端口的仓库：冒号在斜杠之前，不能当成 tag 切
		{"myregistry:5000/app:1.0", "1.1", "myregistry:5000/app:1.1"},
		{"myregistry:5000/app", "2.0", "myregistry:5000/app:2.0"},
		// 摘掉摘要固定后缀
		{"nginx:1.25@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			"1.27", "nginx:1.27"},
		// tag 与当前一致：原样返回，不做无谓改写
		{"nginx:1.25", "1.25", "nginx:1.25"},
		{"nginx", "latest", "nginx"},
		{"nginx:1.25", "", "nginx:1.25"},
	}
	for _, c := range cases {
		ref, err := ParseReference(c.ref)
		if err != nil {
			t.Fatalf("ParseReference(%q) 报错: %v", c.ref, err)
		}
		if got := ref.WithTag(c.tag); got != c.want {
			t.Errorf("%q.WithTag(%q) = %q, 期望 %q", c.ref, c.tag, got, c.want)
		}
	}
}

func TestParseRepoTags(t *testing.T) {
	repo, tag := ParseRepoTags("ghcr.io/foo/bar:v1.2")
	if repo != "ghcr.io/foo/bar" || tag != "v1.2" {
		t.Fatalf("ParseRepoTags = %q, %q", repo, tag)
	}
}

// String() 既是展示形式，也是监控项的唯一键，所以要既好看又归一。
func TestStringNormalization(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"nginx:latest", "nginx:latest"},
		{"nginx", "nginx:latest"},
		// Docker Hub 上这几种写法是同一个镜像，必须归到同一个键，
		// 否则同一个镜像会在列表里出现好几条
		{"library/nginx:latest", "nginx:latest"},
		{"docker.io/nginx:latest", "nginx:latest"},
		{"index.docker.io/library/nginx:latest", "nginx:latest"},
		// 非 Hub 仓库保留完整路径
		{"ghcr.io/foo/bar:1.2.3", "ghcr.io/foo/bar:1.2.3"},
		{"myregistry:5000/app:1.0", "myregistry:5000/app:1.0"},
	}
	for _, c := range cases {
		ref, err := ParseReference(c.in)
		if err != nil {
			t.Fatalf("ParseReference(%q) 报错: %v", c.in, err)
		}
		if got := ref.String(); got != c.want {
			t.Errorf("ParseReference(%q).String() = %q, 期望 %q", c.in, got, c.want)
		}
	}

	// 请求注册表的路径必须保留 library/，不能跟着展示形式一起被削掉
	ref, _ := ParseReference("nginx:latest")
	if ref.FullRepository() != "registry-1.docker.io/library/nginx" {
		t.Fatalf("FullRepository = %q，library/ 前缀不能省", ref.FullRepository())
	}
}

// 镜像 ID 必须和真正的引用区分开：容器的 Config.Image 可能存的是一串 ID，
// 按引用去解析它语法上过得去、语义是假的，查注册表永远查不到。
func TestIsImageID(t *testing.T) {
	ids := []string{
		"c4d252255b7b", // docker ps 里那种短 ID
		"04225415df99",
		"sha256:eeaf36f3057657ebb9447e3465db1a4e785156a9817155c5b020b85edd028d15",
		"EEAF36F3057657EBB9447E3465DB1A4E", // 大小写都认
	}
	for _, s := range ids {
		if !IsImageID(s) {
			t.Errorf("IsImageID(%q) = false，应该认成镜像 ID", s)
		}
	}

	refs := []string{
		"",
		"nginx",
		"nginx:latest",
		"chaitin/safeline-detector:9.4.7",
		"docker.io/library/nginx",
		"ghcr.io/haihaipypy/autodocker:1.12.1",
		"myregistry:5000/app", // 这个冒号是端口，不是 tag，更不能当 ID
		"deadbeefcafz",        // 够长，但带一个非十六进制字母，是仓库名不是 ID
		"abc123",              // 太短
	}
	for _, s := range refs {
		if IsImageID(s) {
			t.Errorf("IsImageID(%q) = true，这不是镜像 ID", s)
		}
	}
}

// Package privatebuild 验证两条构建路径：公共仓库的纯核心 relay，以及私有仓库用源码组装
// （空导入私有模块 + relayapp.Main）得到的带私有模块 relay，并覆盖契约/核心版本不兼容时的失败路径。
//
// 测试会真实调用 go build，需要本地模块缓存里已有公共仓库的依赖（GOPROXY=off，不访问网络）。
package privatebuild

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

type buildEnv struct {
	root    string // 公共仓库根目录
	private string // 临时目录中的私有仓库替身
	bin     string // 产物目录
	env     []string
}

func setup(t *testing.T) *buildEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("builds relay binaries; skipped in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not in PATH")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	b := &buildEnv{root: root, private: filepath.Join(tmp, "private"), bin: filepath.Join(tmp, "bin")}
	copyTree(t, filepath.Join("testdata", "privaterepo"), b.private)
	// 指向本工作区的公共核心；真实私有仓库应 require 固定 tag。
	gomod := filepath.Join(b.private, "go.mod")
	data, err := os.ReadFile(gomod)
	if err != nil {
		t.Fatal(err)
	}
	data = regexp.MustCompile(`(?m)^replace github.com/remote-assist/tool => .*$`).
		ReplaceAll(data, []byte("replace github.com/remote-assist/tool => "+filepath.ToSlash(root)))
	writeFile(t, gomod, data)
	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(b.private, "go.sum"), sum)
	b.env = append(os.Environ(), "GOWORK=off", "GOPROXY=off", "CGO_ENABLED=0")
	return b
}

// build 构建 dir 下的 ./cmd/relay。只有临时私有仓库允许 -mod=mod（需要补全其 go.sum）；
// 公共仓库按默认的只读模式构建，保证测试不会改写工作区的 go.mod/go.sum。
func (b *buildEnv) build(t *testing.T, dir, name string, extra ...string) string {
	t.Helper()
	out := filepath.Join(b.bin, name)
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	args := append([]string{"build", "-o", out}, extra...)
	args = append(args, "./cmd/relay")
	cmd := exec.Command("go", args...)
	cmd.Dir, cmd.Env = dir, b.env
	if dir == b.private {
		cmd.Env = append(append([]string(nil), b.env...), "GOFLAGS=-mod=mod")
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go %s in %s: %v\n%s", strings.Join(args, " "), dir, err, output)
	}
	return out
}

// run 执行二进制并返回合并输出与退出码。
func run(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	default:
		t.Fatalf("run %s: %v", bin, err)
		return "", -1
	}
}

// serveUntil 启动 relay，等待 stderr 出现 want 后确认进程仍在运行，再结束它。
func serveUntil(t *testing.T, bin, moduleConfig, want string) {
	t.Helper()
	cmd := exec.Command(bin, "--plain", "--listen", "127.0.0.1:0", "--audit", "", "--module-config", moduleConfig)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	found := make(chan struct{})
	var logs bytes.Buffer // 只由扫描 goroutine 写入；读取前先等它结束（<-exited）
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			logs.WriteString(sc.Text() + "\n")
			if strings.Contains(sc.Text(), want) {
				close(found)
				_, _ = io.Copy(io.Discard, stderr)
				break
			}
		}
		exited <- cmd.Wait()
	}()
	select {
	case <-found:
	case err := <-exited:
		t.Fatalf("relay exited (%v) before logging %q:\n%s", err, want, logs.String())
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
		t.Fatalf("relay did not log %q:\n%s", want, logs.String())
	}
	select {
	case err := <-exited:
		t.Fatalf("relay stopped after %q: %v", want, err)
	case <-time.After(300 * time.Millisecond):
	}
	_ = cmd.Process.Kill()
	<-exited
}

// 两个用例并行构建，控制在 CI 的单包超时（go test -timeout 120s）之内。
func TestPureCoreAndPrivateModuleBuilds(t *testing.T) {
	t.Parallel()
	b := setup(t)
	core := b.build(t, b.root, "relay-core")
	private := b.build(t, b.private, "relay-private")
	// 测试替身用目录 replace 引用公共核心，没有可信的依赖版本；这里注入核心版本模拟固定 tag 的组装结果。
	privateRelease := b.build(t, b.private, "relay-private-release", "-ldflags", "-X github.com/remote-assist/tool/internal/version.Version=v1.0.0")

	if out, code := run(t, core, "--list-modules"); code != 0 || !strings.Contains(out, "built-in modules: none") {
		t.Fatalf("pure core build: exit %d\n%s", code, out)
	}
	out, code := run(t, private, "--list-modules")
	if code != 0 || !strings.Contains(out, "module demo 1.2.0 source=example.com/private/relay-modules api=v1 min_core=v1.0.0 [unverified") {
		t.Fatalf("private build (unversioned core): exit %d\n%s", code, out)
	}
	out, code = run(t, privateRelease, "--list-modules")
	if code != 0 || !strings.Contains(out, "relay core v1.0.0") || !strings.Contains(out, "module demo 1.2.0") || !strings.Contains(out, "[ok]") {
		t.Fatalf("private build (release core): exit %d\n%s", code, out)
	}

	cfg := filepath.Join(b.bin, "modules.json")
	writeFile(t, cfg, []byte(`{"version":1,"modules":{"demo":{"greeting":"hi","require":{"source":"example.com/private/relay-modules","min_version":"1.2"}}}}`))
	serveUntil(t, privateRelease, cfg, "private demo module started: hi (core v1.0.0)")
	// 同一份配置被纯核心构建加载：私有模块不存在，配置被拒绝，relay 照常服务。
	serveUntil(t, core, cfg, `unknown module "demo"`)
}

func TestIncompatiblePrivateModulesFailExplicitly(t *testing.T) {
	t.Parallel()
	b := setup(t)
	legacy := b.build(t, b.private, "relay-legacy", "-tags", "legacy")
	future := b.build(t, b.private, "relay-future", "-tags", "future", "-ldflags", "-X github.com/remote-assist/tool/internal/version.Version=v1.0.0")

	out, code := run(t, legacy, "--list-modules")
	if code == 0 || !strings.Contains(out, "registration FAILED") || !strings.Contains(out, "built for module API v99") {
		t.Fatalf("legacy --list-modules: exit %d\n%s", code, out)
	}
	out, code = run(t, legacy, "--plain", "--listen", "127.0.0.1:0")
	if code == 0 || !strings.Contains(out, "built-in modules are incompatible") {
		t.Fatalf("legacy relay must refuse to start: exit %d\n%s", code, out)
	}
	out, code = run(t, future, "--list-modules")
	if code == 0 || !strings.Contains(out, "INCOMPATIBLE") || !strings.Contains(out, "requires relay core >= v99.0.0, this relay core is v1.0.0") {
		t.Fatalf("future --list-modules: exit %d\n%s", code, out)
	}
	out, code = run(t, future, "--plain", "--listen", "127.0.0.1:0")
	if code == 0 || !strings.Contains(out, "requires relay core >= v99.0.0") {
		t.Fatalf("future relay must refuse to start: exit %d\n%s", code, out)
	}
	// --version 不受模块组装问题影响，便于排查。
	if out, code := run(t, legacy, "--version"); code != 0 || !strings.Contains(out, "relay-server") {
		t.Fatalf("--version: exit %d\n%s", code, out)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

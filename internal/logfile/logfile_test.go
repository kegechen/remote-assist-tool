package logfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 25, 10, 0, 0, 0, time.Local)

// testOpts 是 auto 模式、上级目录在 t.TempDir() 的参数：PID 固定、别的进程都不在运行、时间固定。
func testOpts(t *testing.T) Options {
	t.Helper()
	return Options{
		Setting: "auto", Mode: "share", Warn: io.Discard,
		tempDir: t.TempDir(), pid: 4242,
		now:   func() time.Time { return testNow },
		alive: func(int) bool { return false },
	}
}

func autoDirOf(o Options) string { return filepath.Join(o.tempDir, "remote-assist-"+userTag()) }

func mustOpen(t *testing.T, o Options) *File {
	t.Helper()
	f, err := Open(o)
	if err != nil {
		t.Fatalf("Open(%q): %v", o.Setting, err)
	}
	if f == nil {
		t.Fatalf("Open(%q) 返回 nil", o.Setting)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func write(t *testing.T, w io.Writer, s string) {
	t.Helper()
	if n, err := w.Write([]byte(s)); n != len(s) || err != nil {
		t.Fatalf("Write(%q) = %d, %v", s, n, err)
	}
}

func stamp(s string) string { return testNow.Format(timeLayout) + s }

func TestSetting(t *testing.T) {
	t.Setenv(EnvVar, "")
	if got := Setting("auto", false); got != "auto" {
		t.Errorf("都没写: %q，要 auto", got)
	}
	t.Setenv(EnvVar, "off")
	if got := Setting("auto", false); got != "off" {
		t.Errorf("没写命令行时用环境变量: %q，要 off", got)
	}
	if got := Setting("/var/log/x.log", true); got != "/var/log/x.log" {
		t.Errorf("命令行优先: %q", got)
	}
	t.Setenv(EnvVar, "  ")
	if got := Setting("auto", false); got != "auto" {
		t.Errorf("环境变量只有空白当没设: %q", got)
	}
}

func TestOpenOff(t *testing.T) {
	for _, s := range []string{"off", "OFF", " Off "} {
		o := testOpts(t)
		o.Setting = s
		f, err := Open(o)
		if f != nil || err != nil {
			t.Fatalf("Open(%q) = %v, %v，要 nil, nil", s, f, err)
		}
		if entries, _ := os.ReadDir(o.tempDir); len(entries) != 0 {
			t.Fatalf("Open(%q) 不该建目录: %v", s, entries)
		}
	}
}

func TestOpenAuto(t *testing.T) {
	t.Chdir(t.TempDir()) // 万一 auto 没认出来被当成相对路径，别写进源码目录
	for _, s := range []string{"auto", "AUTO", ""} {
		o := testOpts(t)
		o.Setting = s
		f := mustOpen(t, o)
		want := filepath.Join(autoDirOf(o), "share-4242.log")
		if f.Path() != want {
			t.Fatalf("Open(%q).Path() = %q，要 %q", s, f.Path(), want)
		}
		write(t, f, "2026/11/25 09:59:59 来自 log\n")
		write(t, f.Status(), "状态\n")
		f.Close()
		if got, want := readFile(t, want), "2026/11/25 09:59:59 来自 log\n"+stamp("状态\n"); got != want {
			t.Fatalf("文件内容 %q，要 %q", got, want)
		}
		if runtime.GOOS != "windows" {
			if fi, _ := os.Stat(autoDirOf(o)); fi.Mode().Perm() != 0o700 {
				t.Errorf("目录权限 %o，要 0700", fi.Mode().Perm())
			}
			if fi, _ := os.Stat(want); fi.Mode().Perm() != 0o600 {
				t.Errorf("文件权限 %o，要 0600", fi.Mode().Perm())
			}
		}
	}
	o := testOpts(t)
	o.Mode = "help"
	if f := mustOpen(t, o); filepath.Base(f.Path()) != "help-4242.log" {
		t.Fatalf("help 模式文件名 %q", filepath.Base(f.Path()))
	}
}

func TestUserTag(t *testing.T) {
	tag := userTag()
	if tag == "" || strings.ContainsAny(tag, `/\:*?"<>| `) {
		t.Fatalf("userTag() = %q：要非空、能直接当文件名", tag)
	}
	if runtime.GOOS != "windows" && tag != fmt.Sprint(os.Getuid()) {
		t.Fatalf("userTag() = %q，要 uid %d", tag, os.Getuid())
	}
	for in, want := range map[string]string{`DOMAIN\alice`: "alice", "bob": "bob", `a b:c`: "a_b_c", "张三": "张三", "": "user", `X\`: "user"} {
		if got := cleanUserName(in); got != want {
			t.Errorf("cleanUserName(%q) = %q，要 %q", in, got, want)
		}
	}
}

// 已有的同名文件（同一个 PID 的旧进程留下的）接着写，不清空。
func TestOpenAppends(t *testing.T) {
	o := testOpts(t)
	os.MkdirAll(autoDirOf(o), 0o700)
	path := filepath.Join(autoDirOf(o), "share-4242.log")
	os.WriteFile(path, []byte("旧的\n"), 0o600)
	f := mustOpen(t, o)
	write(t, f, "新的\n")
	f.Close()
	if got := readFile(t, path); got != "旧的\n新的\n" {
		t.Fatalf("文件内容 %q", got)
	}
}

func TestStatusStamp(t *testing.T) {
	f := mustOpen(t, testOpts(t))
	write(t, f.Status(), "a\n\nb")
	write(t, f.Status(), "c\n")
	write(t, f.Status(), "尾巴")
	write(t, f, "日志")
	write(t, f, "行\n")
	write(t, f, "半行")
	f.Close() // 两种输出攒着的半行都补上换行写出
	want := stamp("a\n") + "\n" + stamp("bc\n") + "日志行\n" + "半行\n" + stamp("尾巴\n")
	if got := readFile(t, f.Path()); got != want {
		t.Fatalf("文件内容\n%q\n要\n%q", got, want)
	}
	// 关了以后再写什么都不做，也不报错。
	write(t, f, "关了\n")
	write(t, f.Status(), "关了\n")
	if got := readFile(t, f.Path()); got != want {
		t.Fatalf("关闭后又写进去了: %q", got)
	}
}

// 颜色、擦行这类控制码不进文件；只有控制码的一行当空行。
func TestStripANSI(t *testing.T) {
	f := mustOpen(t, testOpts(t))
	write(t, f.Status(), "\x1b[1;31m!!! WARNING: NO-AUTH\x1b[0m\n\x1b[2K\n")
	write(t, f, "2026/11/25 09:59:59 \x1b[33m黄\x1b[0m\n")
	f.Close()
	want := stamp("!!! WARNING: NO-AUTH\n") + "\n" + "2026/11/25 09:59:59 黄\n"
	if got := readFile(t, f.Path()); got != want {
		t.Fatalf("文件内容 %q，要 %q", got, want)
	}
}

// 很长一直不换行的输出攒到 maxPending 就当一行写出。
func TestPendingCap(t *testing.T) {
	f := mustOpen(t, testOpts(t))
	write(t, f, strings.Repeat("x", maxPending-1))
	if got := readFile(t, f.Path()); got != "" {
		t.Fatalf("还没攒满就写了 %d 字节", len(got))
	}
	write(t, f, "x")
	if got := readFile(t, f.Path()); got != strings.Repeat("x", maxPending)+"\n" {
		t.Fatalf("攒满后要整段写出并补换行，得 %d 字节", len(got))
	}
}

// assertNoFragments 断言 data 里找不到 code 的任何 4 个字符以上的片段（不分大小写）。
func assertNoFragments(t *testing.T, data, code string) {
	t.Helper()
	up := strings.ToUpper(data)
	for i := 0; i+4 <= len(code); i++ {
		if frag := strings.ToUpper(code[i : i+4]); strings.Contains(up, frag) {
			t.Fatalf("文件里有协助码 %s 的片段 %s:\n%s", code, frag, data)
		}
	}
}

func TestRedact(t *testing.T) {
	const code = "M7QXKZV3WJTA"
	f := mustOpen(t, testOpts(t))
	f.Redact("m7qx-kzv3-wjta") // 登记时的写法不要紧
	f.Redact(code)             // 重复登记无害，只记一份
	f.Redact("ab1")            // 不足 4 个字符的不登记
	f.Redact("")
	for _, l := range []string{
		"协助码: M7QX-KZV3-WJTA\n", "raw M7QXKZV3WJTA\n", "lower m7qx-kzv3-wjta\n",
		"space M7QX KZV3 WJTA\n", "under m7qx_kzv3_wjta\n", "ab1 不动\n",
	} {
		write(t, f, l)
	}
	write(t, f, "拆开写 M7QX-KZ")
	write(t, f, "V3-WJTA 完\n")
	write(t, f.Status(), "状态 M7QXKZV3")
	write(t, f.Status(), "WJTA\n")
	f.Close()
	if len(f.codes) != 1 {
		t.Fatalf("登记了 %d 个，要 1 个", len(f.codes))
	}
	data := readFile(t, f.Path())
	assertNoFragments(t, data, code)
	if n := strings.Count(data, "********"); n != 7 {
		t.Fatalf("要换掉 7 处，换了 %d 处:\n%s", n, data)
	}
	for _, want := range []string{"协助码: ********\n", "ab1 不动\n", "拆开写 ******** 完\n", stamp("状态 ********\n")} {
		if !strings.Contains(data, want) {
			t.Fatalf("文件里没有 %q:\n%s", want, data)
		}
	}
}

// 登记了几个码时都要换（share 换码提示里新旧码都有）。
func TestRedactSeveral(t *testing.T) {
	f := mustOpen(t, testOpts(t))
	f.Redact("M7QX-KZV3-WJTA") // 带分隔符登记的，不带分隔符写出也要换
	f.Redact("m9rb hq4y pcdn")
	write(t, f.Status(), "原协助码 M7QXKZV3WJTA 已变更为 M9RB-HQ4Y-PCDN\n")
	f.Close()
	data := readFile(t, f.Path())
	assertNoFragments(t, data, "M7QXKZV3WJTA")
	assertNoFragments(t, data, "M9RBHQ4YPCDN")
	if want := stamp("原协助码 ******** 已变更为 ********\n"); data != want {
		t.Fatalf("文件内容 %q，要 %q", data, want)
	}
}

func TestRotate(t *testing.T) {
	o := testOpts(t)
	o.maxSize = 64
	f := mustOpen(t, o)
	line := strings.Repeat("a", 15) + "\n" // 16 字节，4 行正好 64
	for i := 0; i < 4; i++ {
		write(t, f, line)
	}
	if _, err := os.Stat(f.Path() + ".1"); err == nil {
		t.Fatal("没超过 64 字节不该轮转")
	}
	write(t, f, strings.Repeat("b", 15)+"\n")
	if got := readFile(t, f.Path()+".1"); got != strings.Repeat(line, 4) {
		t.Fatalf(".1 内容 %q", got)
	}
	if got := readFile(t, f.Path()); got != strings.Repeat("b", 15)+"\n" {
		t.Fatalf("轮转后的新文件 %q", got)
	}
	for i := 0; i < 4; i++ {
		write(t, f, strings.Repeat("c", 15)+"\n")
	}
	if got := readFile(t, f.Path()+".1"); !strings.HasPrefix(got, "bbb") {
		t.Fatalf("第二次轮转要换掉旧的 .1: %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(f.Path()))
	if len(entries) != 2 {
		t.Fatalf("只留一份 .1，目录里却有 %v", entries)
	}

	// 空文件不轮转：一行就超过上限时照写，不把旧的 .1 换成空文件。
	o2 := testOpts(t)
	o2.maxSize = 8
	g := mustOpen(t, o2)
	if err := os.WriteFile(g.Path()+".1", []byte("旧的\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	write(t, g, "超过八个字节的一行\n")
	if got := readFile(t, g.Path()+".1"); got != "旧的\n" {
		t.Fatalf("空文件不该轮转，.1 成了 %q", got)
	}
	if got := readFile(t, g.Path()); got != "超过八个字节的一行\n" {
		t.Fatalf("文件内容 %q", got)
	}

	// 追加打开已有的文件时按已有的大小算（同一个 --log-file 路径反复启动）。
	o3 := testOpts(t)
	o3.Setting = filepath.Join(t.TempDir(), "x.log")
	o3.maxSize = 64
	old := strings.Repeat(line, 4)
	if err := os.WriteFile(o3.Setting, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	h := mustOpen(t, o3)
	write(t, h, "新的一行\n")
	if got := readFile(t, h.Path()+".1"); got != old {
		t.Fatalf("已有 64 字节，再写要先轮转，.1 = %q", got)
	}
}

// 改名失败（这里让 .1 是个非空目录）时清空重写，只警告一次。
func TestRotateRenameFails(t *testing.T) {
	o := testOpts(t)
	o.maxSize = 32
	var warn bytes.Buffer
	o.Warn = &warn
	f := mustOpen(t, o)
	os.MkdirAll(filepath.Join(f.Path()+".1", "x"), 0o700)
	line := strings.Repeat("a", 15) + "\n"
	for i := 0; i < 5; i++ {
		write(t, f, line)
	}
	if got := readFile(t, f.Path()); got != line {
		t.Fatalf("改名失败后要清空重写，文件 %q", got)
	}
	if n := strings.Count(warn.String(), "[日志]"); n != 1 || !strings.Contains(warn.String(), "轮转") {
		t.Fatalf("要警告一次轮转失败，得 %q", warn.String())
	}
	// 之后又写不了（比如磁盘满了）：停写还要单独告诉用户一次。
	f.f.Close()
	write(t, f, line)
	write(t, f, line)
	if n := strings.Count(warn.String(), "[日志]"); n != 2 || !strings.Contains(warn.String(), "不再写日志文件") {
		t.Fatalf("轮转失败之后停写也要警告一次，得 %q", warn.String())
	}
}

func TestWriteFailureWarnsOnce(t *testing.T) {
	o := testOpts(t)
	var warn bytes.Buffer
	o.Warn = &warn
	f := mustOpen(t, o)
	f.f.Close() // 之后的写入都失败
	write(t, f, "一\n")
	write(t, f.Status(), "二\n")
	write(t, f, "三\n")
	if n := strings.Count(warn.String(), "[日志]"); n != 1 {
		t.Fatalf("写失败只警告一次，得 %q", warn.String())
	}
	if !strings.Contains(warn.String(), f.Path()) {
		t.Fatalf("警告里要有文件路径: %q", warn.String())
	}
	if f.f != nil {
		t.Fatal("写失败后要放下文件，之后不再写")
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("坏了") }

// 终端写失败（隐藏窗口启动、stderr 无效）时文件照写。
func TestTee(t *testing.T) {
	f := mustOpen(t, testOpts(t))
	if n, err := Tee(errWriter{}, f).Write([]byte("x\n")); n != 0 || err == nil {
		t.Fatalf("Tee 要返回终端的结果，得 %d, %v", n, err)
	}
	var term bytes.Buffer
	if n, err := Tee(&term, f.Status()).Write([]byte("y\n")); n != 2 || err != nil {
		t.Fatalf("Tee = %d, %v", n, err)
	}
	f.Close()
	if got := readFile(t, f.Path()); got != "x\n"+stamp("y\n") {
		t.Fatalf("文件 %q", got)
	}
	if term.String() != "y\n" {
		t.Fatalf("终端 %q", term.String())
	}
	if w := Tee(&term, nil); w != io.Writer(&term) {
		t.Fatal("没有文件时直接返回终端")
	}
}

func TestOpenExplicitPath(t *testing.T) {
	o := testOpts(t)
	dir := t.TempDir()
	old := filepath.Join(dir, "share-1.log")
	os.WriteFile(old, []byte("x"), 0o600)
	os.Chtimes(old, testNow.Add(-30*24*time.Hour), testNow.Add(-30*24*time.Hour))
	o.Setting = filepath.Join(dir, "my.log")
	f := mustOpen(t, o)
	if f.Path() != o.Setting {
		t.Fatalf("Path() = %q，要 %q", f.Path(), o.Setting)
	}
	write(t, f, "x\n")
	f.Close()
	if got := readFile(t, o.Setting); got != "x\n" {
		t.Fatalf("文件 %q", got)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("写了路径时不清理那个目录")
	}
	if entries, _ := os.ReadDir(o.tempDir); len(entries) != 0 {
		t.Fatalf("写了路径时不建 auto 目录: %v", entries)
	}
	o.Setting = dir // 是个目录：打不开
	if f, err := Open(o); f != nil || err == nil {
		t.Fatalf("路径是目录时要报错，得 %v, %v", f, err)
	}
}

func touch(t *testing.T, path string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestCleanupOld(t *testing.T) {
	o := testOpts(t)
	dir := autoDirOf(o)
	os.MkdirAll(dir, 0o700)
	o.alive = func(pid int) bool { return pid == 2 }
	old := testNow.Add(-maxAge - time.Minute)
	recent := testNow.Add(-maxAge + time.Minute)
	files := map[string]bool{ // 名字 -> 清理后还在不在
		"share-1.log":    false, // 过期、进程不在
		"help-1.log.1":   false,
		"help-2.log":     true,  // 过期，但进程还在运行
		"share-2.log.1":  false, // 进程在运行，但 .1 是轮转下来的旧内容
		"share-3.log":    true,  // 没过期
		"notes.txt":      true,  // 不是本程序起的名字
		"share-x.log":    true,
		"share-4.log.2":  true,
		"share-4242.log": true, // 这次要写的
	}
	for name := range files {
		mod := old
		if name == "share-3.log" {
			mod = recent
		}
		touch(t, filepath.Join(dir, name), mod)
	}
	os.Mkdir(filepath.Join(dir, "share-5.log"), 0o700) // 不是普通文件
	os.Chtimes(filepath.Join(dir, "share-5.log"), old, old)
	mustOpen(t, o)
	for name, keep := range files {
		if exists(filepath.Join(dir, name)) != keep {
			t.Errorf("%s: 清理后还在 = %v，要 %v", name, !keep, keep)
		}
	}
	if !exists(filepath.Join(dir, "share-5.log")) {
		t.Error("不是普通文件的不动")
	}
}

func TestCleanupMaxFiles(t *testing.T) {
	o := testOpts(t)
	o.Mode = "help"
	dir := autoDirOf(o)
	os.MkdirAll(dir, 0o700)
	o.alive = func(pid int) bool { return pid == 200 }
	touch(t, filepath.Join(dir, "help-200.log"), testNow.Add(-time.Hour)) // 最旧，但进程还在运行
	for i := 0; i < 25; i++ {
		touch(t, filepath.Join(dir, fmt.Sprintf("share-%d.log", 100+i)), testNow.Add(-time.Duration(25-i)*time.Minute))
	}
	touch(t, filepath.Join(dir, "notes.txt"), testNow.Add(-2*time.Hour))
	mustOpen(t, o)
	entries, _ := os.ReadDir(dir)
	var ours int
	for _, e := range entries {
		if logName.MatchString(e.Name()) {
			ours++
		}
	}
	if ours != maxFiles {
		t.Fatalf("清理后有 %d 个日志文件，要 %d: %v", ours, maxFiles, entries)
	}
	// 25 + 1（运行中）+ 1（这次的）= 27，要删最旧的 7 个不在运行的。
	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("share-%d.log", 100+i)
		if exists(filepath.Join(dir, name)) != (i >= 7) {
			t.Errorf("%s: 还在 = %v", name, !(i >= 7))
		}
	}
	for _, name := range []string{"help-200.log", "notes.txt", "help-4242.log"} {
		if !exists(filepath.Join(dir, name)) {
			t.Errorf("%s 不该删", name)
		}
	}
}

type fakeInfo struct {
	mode fs.FileMode
}

func (fi fakeInfo) Name() string       { return "d" }
func (fi fakeInfo) Size() int64        { return 0 }
func (fi fakeInfo) Mode() fs.FileMode  { return fi.mode }
func (fi fakeInfo) ModTime() time.Time { return time.Time{} }
func (fi fakeInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi fakeInfo) Sys() any           { return nil }

func TestCheckDir(t *testing.T) {
	mine := func(os.FileInfo) (bool, bool) { return true, true }
	other := func(os.FileInfo) (bool, bool) { return false, true }
	unknown := func(os.FileInfo) (bool, bool) { return false, false }
	for _, tc := range []struct {
		name  string
		mode  fs.FileMode
		owner func(os.FileInfo) (bool, bool)
		want  string // 空串表示通过
	}{
		{"正常", fs.ModeDir | 0o700, mine, ""},
		{"符号链接", fs.ModeSymlink | 0o777, mine, "不是目录"},
		{"目录联接", fs.ModeIrregular, unknown, "不是目录"},
		{"普通文件", 0o700, mine, "不是目录"},
		{"属主不是自己", fs.ModeDir | 0o700, other, "属主"},
		{"权限过宽", fs.ModeDir | 0o755, mine, "权限"},
		{"权限过严", fs.ModeDir | 0o500, mine, "权限"},
		{"查不了属主（Windows）", fs.ModeDir | 0o777, unknown, ""},
	} {
		err := checkDir(fakeInfo{tc.mode}, tc.owner)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v，要通过", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v，要含 %q", tc.name, err, tc.want)
		}
	}
}

// 目录不安全时 Open 报错（调用方警告一次），不写文件。
func TestOpenUnsafeDir(t *testing.T) {
	o := testOpts(t)
	os.WriteFile(autoDirOf(o), []byte("x"), 0o600) // 同名的是个文件
	if f, err := Open(o); f != nil || err == nil || !strings.Contains(err.Error(), autoDirOf(o)) {
		t.Fatalf("同名的是文件: %v, %v", f, err)
	}

	o = testOpts(t)
	target := t.TempDir()
	if err := os.Symlink(target, autoDirOf(o)); err == nil {
		if f, err := Open(o); f != nil || err == nil {
			t.Fatalf("目录是符号链接: %v, %v", f, err)
		}
		if entries, _ := os.ReadDir(target); len(entries) != 0 {
			t.Fatalf("不该往符号链接指向的目录写: %v", entries)
		}
	} else {
		t.Logf("建不了符号链接（%v），跳过这一项", err)
	}

	o = testOpts(t)
	o.owner = func(os.FileInfo) (bool, bool) { return false, true }
	if f, err := Open(o); f != nil || err == nil || !strings.Contains(err.Error(), "属主") {
		t.Fatalf("属主不是自己: %v, %v", f, err)
	}
	if entries, _ := os.ReadDir(autoDirOf(o)); len(entries) != 0 {
		t.Fatalf("目录不安全时不写文件: %v", entries)
	}

	o = testOpts(t)
	os.Mkdir(autoDirOf(o), 0o700)
	os.Chmod(autoDirOf(o), 0o755) // Windows 上报的权限位本来就是 0777
	o.owner = func(os.FileInfo) (bool, bool) { return true, true }
	if f, err := Open(o); f != nil || err == nil || !strings.Contains(err.Error(), "权限") {
		t.Fatalf("权限过宽: %v, %v", f, err)
	}
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("自己在运行")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if processAlive(cmd.Process.Pid) {
		t.Fatalf("退出了的子进程 %d 不在运行", cmd.Process.Pid)
	}
	for _, pid := range []int{0, -1} {
		if processAlive(pid) {
			t.Fatalf("pid %d 不算在运行", pid)
		}
	}
}

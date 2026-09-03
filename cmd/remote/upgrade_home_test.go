package main

import (
	"os"
	"strings"
	"testing"
)

// stash → restore 的往返：升级隔离改写过的 HOME/USERPROFILE 必须还原成 old 进程里的原值。
// 回归点：早先 upgrade-stage 只 replaceEnv 不留原值，继任 share 从此永远把 ~ 指向升级暂存
// 目录，远端所有 exec 子进程（git/ssh/npm）跟着失配。
func TestStashRestoreUpgradeHomeRoundTrip(t *testing.T) {
	env := []string{"PATH=/usr/bin", "HOME=/home/real", "USERPROFILE=C:\\Users\\real"}
	staged := stashOrigHomeEnv(env, "HOME", "USERPROFILE")
	staged = replaceEnvForTest(staged, "HOME", "/tmp/upgrade/home")
	staged = replaceEnvForTest(staged, "USERPROFILE", "/tmp/upgrade/home")

	// 隔离期：子进程看到的是暂存目录，原值只躺在内部变量里。
	if got, _ := lookupEnvIn(staged, "HOME"); got != "/tmp/upgrade/home" {
		t.Fatalf("隔离期 HOME = %q, want /tmp/upgrade/home", got)
	}
	if got, ok := lookupEnvIn(staged, upgradeOrigHomeEnv); !ok || got != "1/home/real" {
		t.Fatalf("%s = %q (ok=%v), want \"1/home/real\"", upgradeOrigHomeEnv, got, ok)
	}

	applyEnvForTest(t, staged)
	restored := restoreUpgradeHomeEnv()

	if len(restored) != 2 {
		t.Fatalf("restored = %v, want HOME 与 USERPROFILE 都还原", restored)
	}
	if got := os.Getenv("HOME"); got != "/home/real" {
		t.Errorf("还原后 HOME = %q, want /home/real", got)
	}
	if got := os.Getenv("USERPROFILE"); got != "C:\\Users\\real" {
		t.Errorf("还原后 USERPROFILE = %q, want C:\\Users\\real", got)
	}
	if _, ok := os.LookupEnv(upgradeOrigHomeEnv); ok {
		t.Errorf("%s 应在还原后清除，否则会被下一次升级当成原值透传", upgradeOrigHomeEnv)
	}
	if _, ok := os.LookupEnv(upgradeOrigUserProfileEnv); ok {
		t.Errorf("%s 应在还原后清除", upgradeOrigUserProfileEnv)
	}
}

// 原本就没有这个变量时，还原必须是 Unsetenv 而不是设成空串——空 HOME 比缺失的 HOME 更糟，
// os.UserHomeDir 会报错，而很多工具会把 ~ 解析成当前目录。
func TestRestoreUpgradeHomeUnsetsWhatWasAbsent(t *testing.T) {
	env := []string{"PATH=/usr/bin", "USERPROFILE=C:\\Users\\real"} // 没有 HOME
	staged := stashOrigHomeEnv(env, "HOME", "USERPROFILE")
	staged = replaceEnvForTest(staged, "HOME", "/tmp/upgrade/home")

	applyEnvForTest(t, staged)
	if os.Getenv("HOME") != "/tmp/upgrade/home" {
		t.Fatalf("测试前置失败：HOME 未进入隔离状态")
	}

	restoreUpgradeHomeEnv()

	if val, ok := os.LookupEnv("HOME"); ok {
		t.Errorf("HOME 原本不存在，还原后应仍不存在，得到 %q", val)
	}
}

// 链式升级：old 还没退出就再升一次时，env 里已经带着上一层的内部变量，
// 此时必须保留最原始的家目录，而不是把当前的隔离目录存成"原值"。
func TestStashKeepsOutermostHomeOnChainedUpgrade(t *testing.T) {
	first := stashOrigHomeEnv([]string{"HOME=/home/real"}, "HOME")
	first = replaceEnvForTest(first, "HOME", "/tmp/upgrade1/home")

	second := stashOrigHomeEnv(first, "HOME")

	if got, _ := lookupEnvIn(second, upgradeOrigHomeEnv); got != "1/home/real" {
		t.Fatalf("%s = %q, want \"1/home/real\"（不能被隔离目录覆盖）", upgradeOrigHomeEnv, got)
	}
}

// 非升级继任者（普通启动）调用还原必须是无操作，不能碰用户的 HOME。
func TestRestoreUpgradeHomeIsNoopWithoutStash(t *testing.T) {
	t.Setenv("HOME", "/home/real")
	os.Unsetenv(upgradeOrigHomeEnv)
	os.Unsetenv(upgradeOrigUserProfileEnv)

	if restored := restoreUpgradeHomeEnv(); len(restored) != 0 {
		t.Fatalf("restored = %v, want 空", restored)
	}
	if got := os.Getenv("HOME"); got != "/home/real" {
		t.Errorf("HOME 被误改成 %q", got)
	}
}

// replaceEnvForTest 复刻 upgrade_linux.go / upgrade_windows.go 里的 replaceEnv。
// 那两个是 build tag 隔离的，本测试文件不带 tag，无法直接调用。
func replaceEnvForTest(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			continue
		}
		out = append(out, item)
	}
	return append(out, prefix+value)
}

// applyEnvForTest 把 KEY=VALUE 切片灌进本进程环境（t.Setenv 负责测试结束后回滚）。
func applyEnvForTest(t *testing.T, env []string) {
	t.Helper()
	// 先清掉本测试关心的几个键，避免宿主环境残留干扰。
	// 借 t.Setenv 登记回滚（它会记住原值并在测试结束时恢复），再立刻 Unsetenv 置为「不存在」。
	for _, key := range []string{"HOME", "USERPROFILE", upgradeOrigHomeEnv, upgradeOrigUserProfileEnv} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	for _, item := range env {
		idx := strings.IndexByte(item, '=')
		if idx <= 0 {
			continue
		}
		t.Setenv(item[:idx], item[idx+1:])
	}
}

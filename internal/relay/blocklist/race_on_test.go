//go:build race

package blocklist

// raceEnabled 表示测试在 -race 下运行。度量型用例（吞吐、锁等待分位数）的阈值按不开 race 的速度定，
// race 检测器会让这些路径慢数倍，所以在 -race 下跳过，避免 CI 的 race 步骤误报。
const raceEnabled = true

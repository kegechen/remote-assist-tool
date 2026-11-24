package blocklist

// 测试里查看主表内部状态的辅助函数：直接读主表，不经过任何对外查询接口。

// trackedIPs 返回主表里全部跟踪条目的键（无序）。
func trackedIPs(l *List) []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	ips := make([]string, 0, len(l.entries))
	for _, e := range l.entries {
		ips = append(ips, e.IP)
	}
	return ips
}

// trackedCount 返回主表条目数。
func trackedCount(l *List) int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.entries)
}

// autoBannedIPs 返回当前生效的自动封禁条目的键（无序）。
func autoBannedIPs(l *List) []string {
	now := l.now()
	l.mu.RLock()
	defer l.mu.RUnlock()
	var ips []string
	for _, e := range l.entries {
		if e.Ban.active(now) && e.Ban.Reason == "auto" {
			ips = append(ips, e.IP)
		}
	}
	return ips
}

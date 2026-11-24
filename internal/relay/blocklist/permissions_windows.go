//go:build windows

package blocklist

func validatePrivateFile(string) error { return nil }

// syncDir Windows 上 MoveFileEx 的元数据持久化由文件系统负责，无对应的目录 fsync。
func syncDir(string) {}

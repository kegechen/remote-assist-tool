//go:build !windows

package blocklist

import (
	"fmt"
	"os"
)

// validatePrivateFile 拒绝 group/other 可访问的文件。
func validatePrivateFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("blocklist file %s must not be accessible by group or others (mode %04o)", path, info.Mode().Perm())
	}
	return nil
}

// syncDir fsync 目录，使 rename 持久化。失败只影响掉电场景，忽略错误。
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

package client

import (
	"io"
	"os"
	"strings"
	"sync"

	"github.com/remote-assist/tool/internal/logfile"
	"github.com/remote-assist/tool/internal/proto"
)

// LogFile 是本进程的日志文件（spec 6.8，实现见 internal/logfile）：Status 写面向用户的状态输出，Redact 登记写进文件前
// 要换成 ******** 的协助码。log 包的输出由调用方（cmd/remote）另外接进去。
type LogFile interface {
	Status() io.Writer
	Redact(code string)
}

var procLog struct {
	sync.RWMutex
	lf LogFile
}

// SetLogFile 设置本进程的日志文件，nil 为不写。要在建 share、help 之前调用：share 建控制台时就取它。
func SetLogFile(lf LogFile) {
	procLog.Lock()
	defer procLog.Unlock()
	procLog.lf = lf
}

func currentLog() LogFile {
	procLog.RLock()
	defer procLog.RUnlock()
	return procLog.lf
}

// logStatus 是日志文件的状态输出，没有日志文件时为 nil。
func logStatus() io.Writer {
	if lf := currentLog(); lf != nil {
		return lf.Status()
	}
	return nil
}

// StatusTee 返回同时写终端 w 和日志文件状态输出的 Writer，返回终端的结果；没有日志文件时就是 w。
func StatusTee(w io.Writer) io.Writer {
	lf := currentLog()
	if lf == nil {
		return w
	}
	return logfile.Tee(w, lf.Status())
}

// stderrLog、stdoutLog 是 help 面向用户的提示的去处：终端，有日志文件时再写一份进去。
func stderrLog() io.Writer { return StatusTee(os.Stderr) }
func stdoutLog() io.Writer { return StatusTee(os.Stdout) }

// redactCode 登记协助码（见 LogFile.Redact）。--no-auth 的固定码是公开的，不登记。
func redactCode(code string) {
	if lf := currentLog(); lf != nil && !strings.EqualFold(code, proto.NoAuthCode) {
		lf.Redact(code)
	}
}

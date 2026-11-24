package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"github.com/remote-assist/tool/internal/client"
	"github.com/remote-assist/tool/internal/logfile"
	"github.com/remote-assist/tool/internal/version"
)

// logFileFlag 定义 --log-file（spec 6.8）。没写时按环境变量 REMOTE_ASSIST_LOG，都没有时是 auto，见 logfile.Setting。
func logFileFlag(fs *flag.FlagSet) *string {
	return fs.String("log-file", "auto", "Also write status output and logs to a file (assist codes masked): auto = <temp dir>/remote-assist-<user>/<share|help>-<pid>.log (rotated at 5 MB, files older than 7 days removed), off, or a file path. Same as env "+logfile.EnvVar+"; this flag wins")
}

// stdoutLog、stderrLog 是这里面向用户的提示的去处：终端，有日志文件时再写一份进去（spec 6.8）。
func stdoutLog() io.Writer { return client.StatusTee(os.Stdout) }
func stderrLog() io.Writer { return client.StatusTee(os.Stderr) }

// openLogFile 按 --log-file（value；没写时看环境变量，见 logfile.Setting）打开本进程的日志文件并接上：log 包的输出、
// client 面向用户的输出都再写一份进去，第一行是版本、模式、PID（main 开头的版本行在打开之前）。路径打到 stderr
// （help --mcp-stdio 的 stdout 是协议通道）。打不开时在 stderr 警告一次、照常运行。
// 返回的函数关掉日志文件并还原 log、client 的输出，可以多次调用。
func openLogFile(fs *flag.FlagSet, value, mode string) (closeLog func()) {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "log-file" {
			set = true
		}
	})
	lf, err := logfile.Open(logfile.Options{Setting: logfile.Setting(value, set), Mode: mode, Warn: os.Stderr})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[日志] %v；不写日志文件（不影响使用）\n", err)
		return func() {}
	}
	if lf == nil { // off
		return func() {}
	}
	fmt.Fprintf(lf.Status(), "remote-assist %s（%s，pid %d）\n", version.Info(), mode, os.Getpid())
	prev := log.Writer()
	log.SetOutput(logfile.Tee(prev, lf))
	client.SetLogFile(lf)
	fmt.Fprintf(os.Stderr, "日志文件：%s\n", lf.Path())
	var once sync.Once
	return func() {
		once.Do(func() {
			client.SetLogFile(nil)
			log.SetOutput(prev)
			lf.Close()
		})
	}
}

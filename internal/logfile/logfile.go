// Package logfile 是 remote share / remote help 终端输出之外默认再写一份的日志文件（spec 6.8）：进程退出、窗口关掉、
// 隐藏窗口启动（通道内升级出来的 share）之后，选节点、清理本机专用地址这类自动行为仍然事后可查。
//
// 写进来的有两种：log 包的输出（Write，自带时间）和面向用户的状态输出（Status，每行补上同样格式的时间）。都按整行写，
// 写之前把登记过的协助码换成 ********（Redact）。写不了只在终端警告一次，之后不再写，不影响运行。
package logfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/remote-assist/tool/internal/logger"
)

const (
	// EnvVar 与命令行 --log-file 同义：auto、off 或日志文件路径（spec 6.8）。
	EnvVar = "REMOTE_ASSIST_LOG"

	defaultMaxSize = 5 << 20 // 到这么大轮转成 .log.1，只留一份
	maxAge         = 7 * 24 * time.Hour
	maxFiles       = 20                     // auto 目录里最多留这么多个日志文件，连同这次的
	maxPending     = 64 << 10               // 一直没有换行时攒到这么多就当一行写出，免得无限攒
	timeLayout     = "2006/01/02 15:04:05 " // 与 log.LstdFlags 的前缀一致
)

// logName 是 auto 目录里本程序起的文件名：<模式>-<pid>.log 和轮转下来的 .log.1。启动清理只动这些。
var logName = regexp.MustCompile(`^(share|help)-([0-9]+)\.log(\.1)?$`)

// ansiSeq 是终端颜色之类的控制序列（--no-auth、--unsafe-exec 的警告带颜色），写进文件前去掉。
var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// Setting 是日志开关的取值：命令行写了 --log-file（flagSet）用它，否则用环境变量 REMOTE_ASSIST_LOG，都没有时是 auto。
func Setting(flagValue string, flagSet bool) string {
	if flagSet {
		return flagValue
	}
	if v := strings.TrimSpace(os.Getenv(EnvVar)); v != "" {
		return v
	}
	return "auto"
}

// Options 是 Open 的参数。
type Options struct {
	Setting string    // auto、off（不分大小写；空串同 auto）或日志文件路径，见 Setting
	Mode    string    // share、help：auto 时文件名是 <Mode>-<pid>.log
	Warn    io.Writer // 终端：中途写不了时的警告写这里，nil 时是 os.Stderr

	// 以下给测试用，零值时用实际的。
	tempDir string                                // auto 目录的上级，空串为 os.TempDir()
	pid     int                                   // 0 为 os.Getpid()
	maxSize int64                                 // 0 为 5 MB
	now     func() time.Time                      // nil 为 time.Now
	alive   func(pid int) bool                    // nil 为 processAlive
	owner   func(os.FileInfo) (owned, known bool) // nil 为 fileOwner
}

// File 是打开的日志文件，并发安全。
type File struct {
	mu      sync.Mutex
	path    string
	f       *os.File // nil：已关闭或写坏了，之后的输出都丢掉
	size    int64
	maxSize int64
	now     func() time.Time
	warn    io.Writer
	codes   []redaction
	logBuf  []byte // log 输出攒着的半行
	statBuf []byte // 状态输出攒着的半行

	// 每种警告只打一次（见 warnOnce）：轮转失败（还在写）和停写分开，停写总要告诉用户一次。
	warnedRotate bool
	warnedStop   bool
}

type redaction struct {
	key string // 去掉分隔符、转成大写的码
	re  *regexp.Regexp
}

// Open 按 o.Setting 打开日志文件。off 时返回 nil, nil。打不开（auto 目录不安全、路径打不开）时返回错误，调用方在终端
// 警告一次、照常运行。auto 时先做启动清理（见 cleanup）。
func Open(o Options) (*File, error) {
	if o.now == nil {
		o.now = time.Now
	}
	if o.maxSize <= 0 {
		o.maxSize = defaultMaxSize
	}
	if o.Warn == nil {
		o.Warn = os.Stderr
	}
	setting := strings.TrimSpace(o.Setting)
	path := setting
	switch {
	case strings.EqualFold(setting, "off"):
		return nil, nil
	case setting == "" || strings.EqualFold(setting, "auto"):
		if o.tempDir == "" {
			o.tempDir = os.TempDir()
		}
		if o.pid == 0 {
			o.pid = os.Getpid()
		}
		if o.alive == nil {
			o.alive = processAlive
		}
		if o.owner == nil {
			o.owner = fileOwner
		}
		dir, err := autoDir(o.tempDir, o.owner)
		if err != nil {
			return nil, err
		}
		name := fmt.Sprintf("%s-%d.log", o.Mode, o.pid)
		cleanup(dir, name, o.now(), o.alive)
		path = filepath.Join(dir, name)
	}
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	fi, err := fh.Stat()
	if err != nil {
		fh.Close()
		return nil, err
	}
	return &File{path: path, f: fh, size: fi.Size(), maxSize: o.maxSize, now: o.now, warn: o.Warn}, nil
}

// autoDir 建好并检查 auto 的目录 <上级>/remote-assist-<uid 或用户名>。
func autoDir(base string, owner func(os.FileInfo) (bool, bool)) (string, error) {
	dir := filepath.Join(base, "remote-assist-"+userTag())
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if err := checkDir(fi, owner); err != nil {
		return "", fmt.Errorf("日志目录 %s %w，不用它（确认无误后可删掉它再启动）", dir, err)
	}
	return dir, nil
}

// checkDir 检查 auto 目录（spec 6.8）：/tmp 所有人可写，别人可能抢先建了同名目录或符号链接。要是真目录（不是符号链接、
// 目录联接）；查得了属主时（Linux、macOS）属主要是当前用户、权限要正好是 0700。Windows 的临时目录本来就是每个用户
// 自己的，查不了属主，不查权限位（Go 在 Windows 上报的权限位也不反映 ACL）。
func checkDir(fi os.FileInfo, owner func(os.FileInfo) (owned, known bool)) error {
	if fi.Mode().Type() != fs.ModeDir {
		return errors.New("不是目录（可能是符号链接）")
	}
	owned, known := owner(fi)
	if !known {
		return nil
	}
	if !owned {
		return errors.New("的属主不是当前用户")
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		return fmt.Errorf("的权限是 %04o，不是 0700", perm)
	}
	return nil
}

// cleanup 是 auto 目录的启动清理（spec 6.8）：删掉修改时间超过 7 天的日志，目录里最多留 maxFiles 个（连同这次要写的
// own，超出时先删最旧的）。只动本程序起的名字（logName）的普通文件；还在运行的进程正在写的 .log 不删——长时间
// 空闲的 share 可能 7 天都没写过一行；删不掉的跳过。
func cleanup(dir, own string, now time.Time, alive func(int) bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type candidate struct {
		name string
		mod  time.Time
	}
	var pool []candidate
	kept := 1 // own
	for _, e := range entries {
		m := logName.FindStringSubmatch(e.Name())
		if m == nil || e.Name() == own || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if pid, _ := strconv.Atoi(m[2]); m[3] == "" && alive(pid) {
			kept++
			continue
		}
		if now.Sub(info.ModTime()) > maxAge {
			if os.Remove(filepath.Join(dir, e.Name())) != nil {
				kept++
			}
			continue
		}
		pool = append(pool, candidate{e.Name(), info.ModTime()})
	}
	slices.SortStableFunc(pool, func(a, b candidate) int { return a.mod.Compare(b.mod) })
	total := kept + len(pool)
	for _, c := range pool {
		if total <= maxFiles {
			break
		}
		if os.Remove(filepath.Join(dir, c.name)) == nil {
			total--
		}
	}
}

// cleanUserName 把用户名变成能放进目录名的样子：去掉域名部分（DOMAIN\user），字母、数字、. _ - 以外的字符换成 _。
func cleanUserName(name string) string {
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, name)
	if name == "" {
		return "user"
	}
	return name
}

// Path 是日志文件的路径。
func (f *File) Path() string { return f.path }

// Write 写一段 log 包的输出（自带时间）。总是返回 len(p), nil：日志文件写不了不能影响 log 的其他去处。
func (f *File) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logBuf = f.feed(f.logBuf, p, false)
	return len(p), nil
}

// Status 返回写面向用户的状态输出的 Writer：每行前面补上与 log 相同格式的时间，空行不补。
func (f *File) Status() io.Writer { return statusWriter{f} }

type statusWriter struct{ f *File }

func (w statusWriter) Write(p []byte) (int, error) {
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	w.f.statBuf = w.f.feed(w.f.statBuf, p, true)
	return len(p), nil
}

// feed 把 p 接在攒着的半行 buf 后面，写出其中的整行，返回剩下的半行。
func (f *File) feed(buf, p []byte, stamp bool) []byte {
	buf = append(buf, p...)
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		f.writeLine(string(buf[:i+1]), stamp)
		buf = buf[i+1:]
	}
	if len(buf) >= maxPending {
		f.writeLine(string(buf)+"\n", stamp)
		buf = nil
	}
	if len(buf) == 0 {
		return nil
	}
	return slices.Clone(buf)
}

// writeLine 写一整行（以换行结尾）：先去掉颜色控制码、脱敏，状态输出补时间，要超过 maxSize 时先轮转。调用方持有 f.mu。
func (f *File) writeLine(line string, stamp bool) {
	if f.f == nil {
		return
	}
	if strings.IndexByte(line, 0x1b) >= 0 {
		line = ansiSeq.ReplaceAllLiteralString(line, "")
	}
	for _, c := range f.codes {
		line = c.re.ReplaceAllLiteralString(line, logger.MaskCode(c.key))
	}
	if stamp && line != "\n" {
		line = f.now().Format(timeLayout) + line
	}
	if f.size > 0 && f.size+int64(len(line)) > f.maxSize {
		f.rotate()
		if f.f == nil {
			return
		}
	}
	n, err := f.f.WriteString(line)
	f.size += int64(n)
	if err != nil {
		f.f.Close()
		f.f = nil
		f.warnOnce(&f.warnedStop, "写日志文件 %s 失败：%v；之后不再写日志文件（不影响使用）", f.path, err)
	}
}

// rotate 把当前文件改名成 .1（替换掉旧的 .1）再新开一个。改名失败（Windows 上被别的进程占着，多见于几个进程共用
// 一个 --log-file 路径）就清空重写，免得文件无限变大。调用方持有 f.mu。
func (f *File) rotate() {
	f.f.Close()
	flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND
	if err := os.Rename(f.path, f.path+".1"); err != nil {
		flags |= os.O_TRUNC
		f.warnOnce(&f.warnedRotate, "日志文件 %s 轮转失败（%v），已清空重写", f.path, err)
	}
	fh, err := os.OpenFile(f.path, flags, 0o600)
	if err != nil {
		f.f = nil
		f.warnOnce(&f.warnedStop, "重新打开日志文件 %s 失败：%v；之后不再写日志文件（不影响使用）", f.path, err)
		return
	}
	f.f, f.size = fh, 0
}

// warnOnce 在终端警告，done 指的那种警告整个进程只打一次。调用方持有 f.mu。
func (f *File) warnOnce(done *bool, format string, args ...any) {
	if *done {
		return
	}
	*done = true
	fmt.Fprintf(f.warn, "[日志] "+format+"\n", args...)
}

// Redact 登记一个协助码：之后写进文件的内容里的这个码都换成 ********（logger.MaskCode），终端上不受影响。
// 按这个码匹配，不分大小写，字符之间允许有 -、空格、_（展示时按 4 位分组）；不从文本里猜哪段像码。
// 不足 4 个字符的不登记。已经攒着的半行也会按它替换（写出时才替换）。
func (f *File) Redact(code string) {
	key := strings.ToUpper(strings.Map(func(r rune) rune {
		if isSeparator(r) {
			return -1
		}
		return r
	}, code))
	if utf8.RuneCountInString(key) < 4 {
		return
	}
	var pat strings.Builder
	pat.WriteString("(?i)")
	for i, r := range key {
		if i > 0 {
			pat.WriteString("[-_ ]?")
		}
		pat.WriteString(regexp.QuoteMeta(string(r)))
	}
	re := regexp.MustCompile(pat.String())
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.codes {
		if c.key == key {
			return
		}
	}
	f.codes = append(f.codes, redaction{key, re})
}

func isSeparator(r rune) bool { return r == '-' || r == ' ' || r == '_' }

// Close 写出攒着的半行并关闭文件。之后的写入什么都不做。
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.logBuf) > 0 {
		f.writeLine(string(f.logBuf)+"\n", false)
	}
	if len(f.statBuf) > 0 {
		f.writeLine(string(f.statBuf)+"\n", true)
	}
	f.logBuf, f.statBuf = nil, nil
	if f.f == nil {
		return nil
	}
	err := f.f.Close()
	f.f = nil
	return err
}

// Tee 返回同时写终端 term 和日志文件 file 的 Writer，返回终端的结果；file 为 nil 时就是 term。
// 不用 io.MultiWriter：终端写失败（隐藏窗口启动、stderr 无效）时它不再写后面的，文件就漏了。
func Tee(term, file io.Writer) io.Writer {
	if file == nil {
		return term
	}
	return tee{term, file}
}

type tee struct{ term, file io.Writer }

func (t tee) Write(p []byte) (int, error) {
	t.file.Write(p)
	return t.term.Write(p)
}

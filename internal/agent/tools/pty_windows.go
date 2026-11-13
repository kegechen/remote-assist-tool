//go:build windows

package tools

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// resolveShell 选 shell：显式指定 > pwsh > powershell.exe > cmd.exe。
func resolveShell(want string) (string, []string, error) {
	cands := []string{"pwsh.exe", "powershell.exe", "cmd.exe"}
	if want != "" {
		cands = []string{want}
	}
	for _, c := range cands {
		p, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		base := strings.ToLower(strings.TrimSuffix(p[strings.LastIndexAny(p, `\/`)+1:], ".exe"))
		switch base {
		case "pwsh", "powershell":
			return p, []string{"-NoLogo"}, nil
		}
		return p, nil, nil
	}
	return "", nil, fmt.Errorf("internal_error: 找不到可用的 shell（试过 %s）", strings.Join(cands, ", "))
}

// conPTYAvailable 检查系统是否提供 ConPTY（Windows 10 1809+）。
func conPTYAvailable() bool {
	k := windows.NewLazySystemDLL("kernel32.dll")
	for _, n := range []string{"CreatePseudoConsole", "ResizePseudoConsole", "ClosePseudoConsole"} {
		if k.NewProc(n).Find() != nil {
			return false
		}
	}
	return true
}

type winPTY struct {
	hpc     windows.Handle
	inW     windows.Handle // 我们写入（键盘输入）
	outR    windows.Handle // 我们读取（终端输出）
	process windows.Handle
	job     windows.Handle

	killOnce   sync.Once
	finishOnce sync.Once
	closeOnce  sync.Once
	readEnded  chan struct{}
	readMu     sync.Mutex
	readClosed bool
	exited     chan struct{}
	code       int
	pid        uint32 // 创建时记下；process 句柄会在 Close 后被关掉，之后不能再从句柄取 PID
}

var enableCtrlCOnce sync.Once

// enableCtrlCForChildren 清除本进程的“忽略 Ctrl+C”标志。该标志会被子进程继承：share 若由
// 忽略了 Ctrl+C 的父进程（某些终端/IDE/自动化宿主）拉起，shell 里的 ping/长命令就收不到 Ctrl+C，
// 用户在 webui 终端里按 Ctrl+C 将无法中断。清除它只是恢复默认的 Ctrl+C 处理。
func enableCtrlCForChildren() {
	enableCtrlCOnce.Do(func() {
		windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler").Call(0, 0)
	})
}

func startPTY(spec ptySpec) (ptyProc, error) {
	if !conPTYAvailable() {
		return nil, fmt.Errorf("pty_unsupported: 被控端系统不支持 ConPTY（需要 Windows 10 1809 或更新版本）")
	}
	enableCtrlCForChildren()
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		return nil, fmt.Errorf("internal_error: 创建输入管道失败: %w", err)
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(inW)
		return nil, fmt.Errorf("internal_error: 创建输出管道失败: %w", err)
	}
	closeAll := func(hs ...windows.Handle) {
		for _, h := range hs {
			windows.CloseHandle(h)
		}
	}
	var hpc windows.Handle
	size := windows.Coord{X: int16(spec.Cols), Y: int16(spec.Rows)}
	if err := windows.CreatePseudoConsole(size, inR, outW, 0, &hpc); err != nil {
		closeAll(inR, inW, outR, outW)
		return nil, fmt.Errorf("pty_unsupported: CreatePseudoConsole 失败: %w", err)
	}
	// 伪控制台持有自己的副本，我们这侧的 pty 端句柄立即关闭。
	closeAll(inR, outW)

	fail := func(err error) (ptyProc, error) {
		windows.ClosePseudoConsole(hpc)
		closeAll(inW, outR)
		return nil, err
	}

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return fail(fmt.Errorf("internal_error: 属性列表: %w", err))
	}
	defer attrs.Delete()
	// 属性值就是 HPCON 句柄本身（不是指向它的指针）。
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&hpc)), unsafe.Sizeof(hpc)); err != nil {
		return fail(fmt.Errorf("internal_error: 绑定伪控制台: %w", err))
	}

	cmdline := windows.ComposeCommandLine(append([]string{spec.Shell}, spec.Args...))
	cmdlineW, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return fail(fmt.Errorf("bad args: shell: %w", err))
	}
	var cwdW *uint16
	if spec.Cwd != "" {
		if cwdW, err = windows.UTF16PtrFromString(spec.Cwd); err != nil {
			return fail(fmt.Errorf("bad args: cwd: %w", err))
		}
	}
	var envW *uint16
	if len(spec.Env) > 0 {
		env := os.Environ()
		for k, v := range spec.Env {
			env = append(env, k+"="+v)
		}
		block := make([]uint16, 0, 4096)
		for _, e := range env {
			u, err := windows.UTF16FromString(e)
			if err != nil {
				continue
			}
			block = append(block, u...) // 含结尾 0
		}
		block = append(block, 0)
		envW = &block[0]
	}

	var si windows.StartupInfoEx
	si.StartupInfo.Cb = uint32(unsafe.Sizeof(si))
	// 标准句柄显式置空：否则父进程的 stdio 被重定向（管道/文件）时，子进程会继承它们而不是伪控制台。
	si.StartupInfo.Flags = windows.STARTF_USESTDHANDLES
	si.ProcThreadAttributeList = attrs.List()
	var pi windows.ProcessInformation
	// CREATE_SUSPENDED：先入 Job 再恢复，保证孙进程不会在入 Job 前漏出去。
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED)
	if err := windows.CreateProcess(nil, cmdlineW, nil, nil, false, flags, envW, cwdW, &si.StartupInfo, &pi); err != nil {
		return fail(fmt.Errorf("internal_error: 启动 shell 失败: %w", err))
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err == nil {
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
		if err == nil {
			err = windows.AssignProcessToJobObject(job, pi.Process)
		}
	}
	if err != nil {
		if job != 0 {
			windows.CloseHandle(job)
		}
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		windows.CloseHandle(pi.Thread)
		return fail(fmt.Errorf("internal_error: 创建 Job Object 失败: %w", err))
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		windows.TerminateJobObject(job, 1)
		windows.CloseHandle(job)
		windows.CloseHandle(pi.Process)
		windows.CloseHandle(pi.Thread)
		return fail(fmt.Errorf("internal_error: 恢复 shell 线程失败: %w", err))
	}
	windows.CloseHandle(pi.Thread)

	p := &winPTY{hpc: hpc, inW: inW, outR: outR, process: pi.Process, job: job, pid: pi.ProcessId,
		readEnded: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		windows.WaitForSingleObject(pi.Process, windows.INFINITE)
		var c uint32
		windows.GetExitCodeProcess(pi.Process, &c)
		p.code = int(int32(c))
		close(p.exited)
	}()
	return p, nil
}

func (p *winPTY) Read(b []byte) (int, error) {
	var n uint32
	err := windows.ReadFile(p.outR, b, &n, nil)
	if err != nil {
		p.readMu.Lock()
		if !p.readClosed {
			p.readClosed = true
			close(p.readEnded)
		}
		p.readMu.Unlock()
		if n > 0 {
			return int(n), nil
		}
		return 0, io.EOF
	}
	return int(n), nil
}

func (p *winPTY) Write(b []byte) (int, error) {
	total := 0
	for total < len(b) {
		var n uint32
		if err := windows.WriteFile(p.inW, b[total:], &n, nil); err != nil {
			return total, err
		}
		total += int(n)
	}
	return total, nil
}

func (p *winPTY) Resize(cols, rows uint16) error {
	return windows.ResizePseudoConsole(p.hpc, windows.Coord{X: int16(cols), Y: int16(rows)})
}

func (p *winPTY) Wait() int {
	<-p.exited
	return p.code
}

// Kill 终止 Job 内全部进程（整棵进程树，含 shell 派生的后台/孙进程）。
func (p *winPTY) Kill() {
	p.killOnce.Do(func() {
		_ = windows.TerminateJobObject(p.job, 1)
	})
}

// Finish 关闭伪控制台，让 conhost 刷出残余输出并关闭输出管道（Read 得到 EOF）。
// ClosePseudoConsole 在旧系统上会等输出被读走，所以放 goroutine，由 Run 侧的读循环负责排空。
func (p *winPTY) Finish() {
	p.finishOnce.Do(func() {
		go windows.ClosePseudoConsole(p.hpc)
	})
}

func (p *winPTY) Close() {
	p.Kill()
	p.Finish()
	p.closeOnce.Do(func() {
		// 句柄在读循环结束（或超时）后再关，避免关掉一个正被 ReadFile 阻塞的句柄。
		go func() {
			select {
			case <-p.readEnded:
			case <-time.After(5 * time.Second):
			}
			windows.CloseHandle(p.inW)
			windows.CloseHandle(p.outR)
			windows.CloseHandle(p.process)
			windows.CloseHandle(p.job) // KILL_ON_JOB_CLOSE：兜底再杀一次
		}()
	})
}

// Pid 返回 shell 进程号（测试与诊断用）。
func (p *winPTY) Pid() int {
	id, _ := windows.GetProcessId(p.process)
	return int(id)
}

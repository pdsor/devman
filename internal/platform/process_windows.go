//go:build windows

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Access rights and flags not exported by x/sys/windows.
const (
	threadSuspendResume = 0x0002
	swHide              = 0
	utf8CodePage        = 65001
)

var (
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	user32                 = windows.NewLazySystemDLL("user32.dll")
	procAllocConsole       = kernel32.NewProc("AllocConsole")
	procGetConsoleCP       = kernel32.NewProc("GetConsoleCP")
	procGetConsoleOutputCP = kernel32.NewProc("GetConsoleOutputCP")
	procSetConsoleOutputCP = kernel32.NewProc("SetConsoleOutputCP")
	procGetConsoleWindow   = kernel32.NewProc("GetConsoleWindow")
	procSetConsoleCtrlHdlr = kernel32.NewProc("SetConsoleCtrlHandler")
	procShowWindow         = user32.NewProc("ShowWindow")
)

// sysProcess holds the Job Object that owns the service's process tree.
//
// The job is created with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so even if the
// daemon is killed without cleaning up, Windows terminates the whole tree when
// the last handle closes. Descendants cannot leave the job.
type sysProcess struct {
	job      windows.Handle
	release_ sync.Once
}

// startPlatform creates the job, starts the child suspended so nothing can spawn
// before it is contained, assigns it to the job, then resumes it.
func startPlatform(cmd *exec.Cmd) (*sysProcess, error) {
	job, err := createKillOnCloseJob()
	if err != nil {
		return nil, fmt.Errorf("cannot create job object: %w", err)
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// CREATE_NEW_PROCESS_GROUP makes the child its own group so CTRL_BREAK can
	// be targeted at it (and its descendants) without touching the daemon.
	// CREATE_SUSPENDED closes the race window between spawn and job assignment.
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED

	if err := cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	pid := uint32(cmd.Process.Pid)

	handle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_INFORMATION,
		false, pid)
	if err != nil {
		terminateAndReap(cmd)
		windows.CloseHandle(job)
		return nil, fmt.Errorf("cannot open child process: %w", err)
	}
	defer windows.CloseHandle(handle)

	if err := windows.AssignProcessToJobObject(job, handle); err != nil {
		terminateAndReap(cmd)
		windows.CloseHandle(job)
		return nil, fmt.Errorf("cannot assign process to job object: %w", err)
	}
	if err := resumeProcess(pid); err != nil {
		terminateAndReap(cmd)
		windows.CloseHandle(job)
		return nil, fmt.Errorf("cannot resume child process: %w", err)
	}
	return &sysProcess{job: job}, nil
}

func terminateAndReap(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}

func createKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// resumeProcess resumes every thread of a process created suspended.
func resumeProcess(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return err
	}
	resumed := 0
	for {
		if entry.OwnerProcessID == pid {
			thread, openErr := windows.OpenThread(threadSuspendResume, false, entry.ThreadID)
			if openErr == nil {
				if _, resumeErr := windows.ResumeThread(thread); resumeErr == nil {
					resumed++
				}
				windows.CloseHandle(thread)
			}
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			break
		}
	}
	if resumed == 0 {
		return fmt.Errorf("no threads resumed for pid %d", pid)
	}
	return nil
}

// signalGraceful sends CTRL_BREAK to the child's process group.
//
// GenerateConsoleCtrlEvent only reaches processes attached to the caller's
// console, which is why the daemon calls EnsureConsole at startup. Console
// applications translate CTRL_BREAK into a clean shutdown; anything that
// ignores it is force-killed after the graceful timeout.
func (s *sysProcess) signalGraceful(pid int) error {
	if !hasConsole() {
		return fmt.Errorf("no console attached: CTRL_BREAK cannot be delivered")
	}
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
}

// killTree terminates every process in the job.
func (s *sysProcess) killTree(pid int) error {
	if s.job != 0 {
		if err := windows.TerminateJobObject(s.job, 1); err == nil {
			return nil
		}
	}
	// Fall back to the direct child only if the job is gone.
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return windows.TerminateProcess(handle, 1)
}

// release closes the job handle. Because of KILL_ON_JOB_CLOSE this must only
// happen after the tree has exited.
func (s *sysProcess) release() {
	s.release_.Do(func() {
		if s.job != 0 {
			windows.CloseHandle(s.job)
			s.job = 0
		}
	})
}

// signalName has no Windows equivalent: processes report exit codes only.
func signalName(*os.ProcessState) string { return "" }

// UseUTF8Console makes the attached console read DevMan's output as UTF-8 and
// returns the function that puts it back.
//
// DevMan writes UTF-8 everywhere, a service's own log lines included. A Windows
// console defaults to the system's legacy code page — 936 on a Chinese install,
// 932 on a Japanese one — which turns every multi-byte character into mojibake,
// so `devman logs` becomes unreadable for exactly the users whose services print
// non-ASCII. The previous code page is restored on the way out: the console
// belongs to the user's shell and outlives this process.
func UseUTF8Console() func() {
	// Only when output really goes to the console. A redirected stdout carries
	// raw UTF-8 bytes and the code page has no say in how they are decoded, so
	// touching it would be a side effect on the user's shell for no gain.
	var mode uint32
	if err := windows.GetConsoleMode(windows.Handle(os.Stdout.Fd()), &mode); err != nil {
		return func() {}
	}
	previous, _, _ := procGetConsoleOutputCP.Call()
	if previous == 0 || previous == utf8CodePage {
		return func() {}
	}
	if ret, _, _ := procSetConsoleOutputCP.Call(utf8CodePage); ret == 0 {
		return func() {}
	}
	return func() { procSetConsoleOutputCP.Call(previous) }
}

// EnsureConsole guarantees the daemon has a console so that CTRL_BREAK can be
// delivered to child process groups. A console allocated here is hidden
// immediately so no window appears.
//
// Without a console, graceful shutdown on Windows degrades to a force kill of
// the job object, which is why the daemon calls this during startup.
func EnsureConsole() error {
	if hasConsole() {
		return nil
	}
	if ret, _, err := procAllocConsole.Call(); ret == 0 {
		// ERROR_ACCESS_DENIED means a console is already attached, which is all
		// we actually need.
		if errno, ok := err.(syscall.Errno); ok && errno == windows.ERROR_ACCESS_DENIED && hasConsole() {
			return nil
		}
		return fmt.Errorf("AllocConsole failed: %w", err)
	}
	if window := consoleWindow(); window != 0 {
		procShowWindow.Call(window, swHide)
	}
	// Ignore CTRL+C in the daemon itself; child groups are signalled explicitly.
	procSetConsoleCtrlHdlr.Call(0, 1)
	return nil
}

// HasConsole reports whether graceful CTRL_BREAK delivery is possible.
func HasConsole() bool { return hasConsole() }

// hasConsole detects an attached console without assuming it has a window.
// A ConPTY-hosted console has no window, and under `go test` the standard
// handles are pipes, so the console code page is the reliable signal.
func hasConsole() bool {
	if cp, _, _ := procGetConsoleCP.Call(); cp != 0 {
		return true
	}
	return consoleWindow() != 0
}

func consoleWindow() uintptr {
	window, _, _ := procGetConsoleWindow.Call()
	return window
}

// Alive reports whether a PID currently exists.
func Alive(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}

// Inspect reads the identity of a live PID for reconciliation.
func Inspect(pid int) (ProcessInfo, error) {
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ProcessInfo{}, err
	}
	defer windows.CloseHandle(handle)

	info := ProcessInfo{PID: pid}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err == nil {
		info.StartedAt = time.Unix(0, creation.Nanoseconds()).UTC()
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(handle, 0, &buf[0], &size); err == nil {
		info.Executable = windows.UTF16ToString(buf[:size])
		info.Name = filepath.Base(info.Executable)
	}
	return info, nil
}

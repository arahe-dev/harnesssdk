//go:build windows

package harnesssdk

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// On Windows the process tree is a Job Object. The CLI starts suspended, is
// placed in a new job with kill-on-close, then resumed, so every process it
// starts (cmd.exe shims, node, MCP servers, shell commands) is in the job
// from its first instruction. Killing the job kills the tree.

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
	procCreateToolhelp32Snapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
	procThread32First            = kernel32.NewProc("Thread32First")
	procThread32Next             = kernel32.NewProc("Thread32Next")
	procOpenThread               = kernel32.NewProc("OpenThread")
	procResumeThread             = kernel32.NewProc("ResumeThread")
)

const (
	createSuspended                 = 0x00000004
	jobObjectExtendedLimitInfoClass = 9
	jobObjectLimitKillOnJobClose    = 0x00002000
	processSetQuota                 = 0x0100
	processTerminate                = 0x0001
	th32csSnapThread                = 0x00000004
	threadSuspendResume             = 0x0002
)

type jobBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount, WriteOperationCount, OtherOperationCount uint64
	ReadTransferCount, WriteTransferCount, OtherTransferCount    uint64
}

type jobExtendedLimitInformation struct {
	BasicLimitInformation jobBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type threadEntry32 struct {
	Size           uint32
	Usage          uint32
	ThreadID       uint32
	OwnerProcessID uint32
	BasePri        int32
	DeltaPri       int32
	Flags          uint32
}

type tree struct {
	cmd *exec.Cmd
	mu  sync.Mutex
	job syscall.Handle
}

func newTree(cmd *exec.Cmd) *tree {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createSuspended}
	return &tree{cmd: cmd}
}

// started puts the suspended process in a job and resumes it. If the job
// cannot be made (a parent job can forbid it), the process still runs, kill
// falls back to taskkill /T, which walks the process tree by parent id, and
// the returned warning says so.
func (t *tree) started() (warning string, err error) {
	pid := uint32(t.cmd.Process.Pid)
	if jerr := t.contain(pid); jerr != nil {
		warning = fmt.Sprintf("no job object for pid %d (%v): cancelling falls back to taskkill /T, which can miss orphaned descendants, and processes the CLI leaves running when it exits are not cleaned up", pid, jerr)
	}
	return warning, resumeProcess(pid)
}

func (t *tree) contain(pid uint32) error {
	job, err := newKillOnCloseJob()
	if err != nil {
		return fmt.Errorf("create job: %w", err)
	}
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, pid)
	if err != nil {
		syscall.CloseHandle(job)
		return fmt.Errorf("open process: %w", err)
	}
	defer syscall.CloseHandle(h)
	if r, _, err := procAssignProcessToJobObject.Call(uintptr(job), uintptr(h)); r == 0 {
		syscall.CloseHandle(job)
		return fmt.Errorf("assign to job: %w", err)
	}
	t.job = job
	return nil
}

func (t *tree) kill() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job != 0 {
		procTerminateJobObject.Call(uintptr(t.job), 1)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(t.cmd.Process.Pid)).Run() != nil {
		t.cmd.Process.Kill()
	}
}

// close releases the job; kill-on-close ends anything still running in it.
func (t *tree) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job != 0 {
		syscall.CloseHandle(t.job)
		t.job = 0
	}
}

func newKillOnCloseJob() (syscall.Handle, error) {
	r, _, err := procCreateJobObjectW.Call(0, 0)
	if r == 0 {
		return 0, err
	}
	job := syscall.Handle(r)
	var info jobExtendedLimitInformation
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	ok, _, err := procSetInformationJobObject.Call(uintptr(job), jobObjectExtendedLimitInfoClass,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if ok == 0 {
		syscall.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// resumeProcess resumes every thread of a process started suspended.
func resumeProcess(pid uint32) error {
	r, _, err := procCreateToolhelp32Snapshot.Call(th32csSnapThread, 0)
	if syscall.Handle(r) == syscall.InvalidHandle {
		return err
	}
	snap := syscall.Handle(r)
	defer syscall.CloseHandle(snap)
	te := threadEntry32{Size: uint32(unsafe.Sizeof(threadEntry32{}))}
	resumed := false
	for ok, _, _ := procThread32First.Call(uintptr(snap), uintptr(unsafe.Pointer(&te))); ok != 0; ok, _, _ = procThread32Next.Call(uintptr(snap), uintptr(unsafe.Pointer(&te))) {
		if te.OwnerProcessID != pid {
			continue
		}
		h, _, err := procOpenThread.Call(threadSuspendResume, 0, uintptr(te.ThreadID))
		if h == 0 {
			return err
		}
		procResumeThread.Call(h)
		syscall.CloseHandle(syscall.Handle(h))
		resumed = true
	}
	if !resumed {
		return syscall.Errno(1168) // ERROR_NOT_FOUND
	}
	return nil
}

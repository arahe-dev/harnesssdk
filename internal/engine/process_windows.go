//go:build windows

package engine

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	processQueryLimitedInformation  = 0x1000
	synchronize                     = 0x00100000
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

	// Without a job: the CLI and every descendant seen while it ran, each
	// with an open handle. An open handle keeps Windows from reusing the
	// process id, so a tracked id always names the tracked process, and a
	// grandchild whose own parent already exited can still be found through
	// that parent's id.
	root      tracked
	known     map[uint32]tracked
	stopTrack chan struct{}
	trackDone chan struct{}
}

// tracked is a process with its creation time (FILETIME as Unix
// nanoseconds) and an open handle.
type tracked struct {
	pid    uint32
	birth  int64
	handle syscall.Handle
}

// forceNoJob makes started skip the job, so tests reach the fallback.
var forceNoJob bool

// newProcess starts the CLI suspended, so it can be put in a job before it
// runs. Inherited files are not supported on Windows; the job ties the
// tree's life to this process instead.
func newProcess(path string, args []string, _ []*os.File) (*exec.Cmd, *tree, error) {
	cmd := exec.Command(path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createSuspended}
	return cmd, &tree{cmd: cmd}, nil
}

// guard is not used on Windows.
func guard([]string) int { return 127 }

// started puts the suspended process in a job and resumes it. If the job
// cannot be made (a parent job can forbid it), the process still runs and
// its tree is tracked by parent process ids instead, best effort: a process
// that starts a child and exits within one tracking interval can hide that
// child. The warning says so.
func (t *tree) started() (warning string, err error) {
	pid := uint32(t.cmd.Process.Pid)
	jerr := errors.New("disabled for a test")
	if !forceNoJob {
		jerr = t.contain(pid)
	}
	if jerr != nil {
		warning = fmt.Sprintf("no job object for pid %d (%v): the process tree is tracked by parent process ids instead, best effort", pid, jerr)
		t.known = map[uint32]tracked{}
		if h, birth, ok := openTracked(pid, 0, time.Now().UnixNano()); ok {
			t.root = tracked{pid, birth, h}
		}
		t.stopTrack, t.trackDone = make(chan struct{}), make(chan struct{})
		go t.track()
	}
	return warning, resumeProcess(pid)
}

// track records the CLI's descendants while it runs.
func (t *tree) track() {
	defer close(t.trackDone)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		t.mu.Lock()
		t.discover()
		t.mu.Unlock()
		select {
		case <-t.stopTrack:
			return
		case <-tick.C:
		}
	}
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
	if t.known == nil {
		t.cmd.Process.Kill()
		return
	}
	if t.root.handle != 0 {
		syscall.TerminateProcess(t.root.handle, 1)
	}
	t.killDescendants()
}

// close releases the job, and kill-on-close ends anything still running in
// it; without a job, the CLI's leftover descendants are killed and every
// tracked handle is closed.
func (t *tree) close() {
	if t.stopTrack != nil {
		close(t.stopTrack)
		<-t.trackDone
		t.stopTrack = nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job != 0 {
		syscall.CloseHandle(t.job)
		t.job = 0
		return
	}
	if t.known == nil {
		return
	}
	t.killDescendants()
	for _, p := range t.known {
		syscall.CloseHandle(p.handle)
	}
	t.known = map[uint32]tracked{}
	if t.root.handle != 0 {
		syscall.CloseHandle(t.root.handle)
		t.root.handle = 0
	}
}

// killDescendants kills every descendant, current or seen while the CLI
// ran, through the handles opened when each was found. Two passes catch
// processes started while the first ran. Caller holds mu.
func (t *tree) killDescendants() {
	for range 2 {
		t.discover()
		for _, p := range t.known {
			syscall.TerminateProcess(p.handle, 1)
		}
	}
}

// discover walks a process snapshot from the root and from every tracked
// process, and starts tracking each new child: one born after its parent
// and before the snapshot (a process born later reused an id that was free
// when the snapshot was taken). Caller holds mu.
func (t *tree) discover() {
	if t.root.handle == 0 {
		return
	}
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	taken := time.Now().UnixNano()
	children := map[uint32][]uint32{}
	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err := syscall.Process32First(snap, &pe); err == nil; err = syscall.Process32Next(snap, &pe) {
		if pe.ProcessID != pe.ParentProcessID {
			children[pe.ParentProcessID] = append(children[pe.ParentProcessID], pe.ProcessID)
		}
	}
	syscall.CloseHandle(snap)
	queue := []tracked{t.root}
	for _, p := range t.known {
		queue = append(queue, p)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range children[cur.pid] {
			if _, ok := t.known[c]; ok || c == t.root.pid {
				continue
			}
			h, birth, ok := openTracked(c, cur.birth, taken)
			if !ok {
				continue
			}
			p := tracked{c, birth, h}
			t.known[c] = p
			queue = append(queue, p)
		}
	}
}

// openTracked opens a process for tracking if it was born in [after,
// before]; the handle it returns keeps the id from being reused.
func openTracked(pid uint32, after, before int64) (syscall.Handle, int64, bool) {
	h, err := syscall.OpenProcess(processTerminate|processQueryLimitedInformation|synchronize, false, pid)
	if err != nil {
		return 0, 0, false
	}
	var created, exited, kernel, user syscall.Filetime
	if syscall.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		syscall.CloseHandle(h)
		return 0, 0, false
	}
	birth := created.Nanoseconds()
	if birth < after || birth > before {
		syscall.CloseHandle(h)
		return 0, 0, false
	}
	return h, birth, true
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

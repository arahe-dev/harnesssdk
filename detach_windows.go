//go:build windows

package harnesssdk

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unicode/utf16"
)

const (
	createNewProcessGroup  = 0x00000200
	createBreakawayFromJob = 0x01000000
	createNoWindow         = 0x08000000
)

// detach starts exe outside the Job Object of the starting command, if it
// has one: a shell tool that kills its job when a command ends (or times
// out) would otherwise kill the worker with it. Breaking away is tried
// first; a job that forbids it (CreateProcess then fails) is escaped through
// WMI's Win32_Process.Create, whose processes are not associated with the
// caller's job (Microsoft Learn, "Job Objects"). Set HARNESS_DETACH=wmi to
// use WMI directly.
func detach(exe string, args []string, dir string) error {
	if os.Getenv("HARNESS_DETACH") != "wmi" {
		cmd := exec.Command(exe, args...)
		cmd.Dir = dir
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createBreakawayFromJob | createNewProcessGroup | createNoWindow}
		if err := cmd.Start(); err == nil {
			return cmd.Process.Release()
		}
	}
	return detachWMI(exe, args, dir)
}

func detachWMI(exe string, args []string, dir string) error {
	parts := []string{syscall.EscapeArg(exe)}
	for _, a := range args {
		parts = append(parts, syscall.EscapeArg(a))
	}
	script := fmt.Sprintf(
		"$si = New-CimInstance -ClassName Win32_ProcessStartup -ClientOnly -Property @{ShowWindow=[uint16]0}; "+
			"$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine=%s; CurrentDirectory=%s; ProcessStartupInformation=$si}; "+
			"if ($r.ReturnValue -ne 0) { [Console]::Error.WriteLine('Win32_Process.Create returned ' + $r.ReturnValue) }; "+
			"exit [int]$r.ReturnValue",
		psQuote(strings.Join(parts, " ")), psQuote(dir))
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encodePS(script)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Win32_Process.Create: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// psQuote makes a PowerShell single-quoted string literal.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// encodePS encodes a script for powershell -EncodedCommand (UTF-16LE,
// base64), which avoids every layer of command-line quoting.
func encodePS(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	return base64.StdEncoding.EncodeToString(b)
}

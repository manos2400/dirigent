/*
 * MIT License
 *
 * Copyright (c) 2024 EASL
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */

// fc-launcher is a combined init + PID-1 launcher for Firecracker micro-VMs.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"unsafe"
)

const entrypointPath = "/.fc-entrypoint.json"

type fcEntrypoint struct {
	Cmd     []string `json:"cmd"`
	Workdir string   `json:"workdir"`
	Env     []string `json:"env"`
}

var logFile *os.File

func initConsole() {
	// Try /dev/console first (Firecracker creates it via devtmpfs),
	// then /dev/ttyS0 (serial console if 8250.nr_uarts > 0).
	for _, path := range []string{"/dev/console", "/dev/ttyS0"} {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err == nil {
			logFile = f
			return
		}
	}
}

func logMsg(msg string, args ...interface{}) {
	s := fmt.Sprintf("fc-launcher: "+msg+"\n", args...)
	if logFile != nil {
		logFile.WriteString(s)
	}
	os.Stderr.WriteString(s)
}

func fatal(msg string, args ...interface{}) {
	logMsg(msg, args...)
	// Give the serial console time to flush before we die
	syscall.Sync()
	os.Exit(1)
}

func setupOverlay() {
	// Mount essential virtual filesystems — ignore errors if kernel
	// already mounted them (e.g. devtmpfs auto-mount).
	os.MkdirAll("/proc", 0755)
	os.MkdirAll("/sys", 0755)
	os.MkdirAll("/dev", 0755)
	syscall.Mount("proc", "/proc", "proc", 0, "")
	syscall.Mount("sysfs", "/sys", "sysfs", 0, "")
	syscall.Mount("devtmpfs", "/dev", "devtmpfs", 0, "")

	// Now that /dev is mounted, try to open a console for logging
	initConsole()

	// Create overlay structure in tmpfs
	os.MkdirAll("/mnt", 0755)
	if err := syscall.Mount("tmpfs", "/mnt", "tmpfs", 0, ""); err != nil {
		fatal("mount tmpfs on /mnt: %v", err)
	}
	os.MkdirAll("/mnt/upper", 0755)
	os.MkdirAll("/mnt/work", 0755)
	os.MkdirAll("/mnt/merged", 0755)

	// Build overlay: read-only root as lower, tmpfs as upper
	if err := syscall.Mount("overlay", "/mnt/merged", "overlay", 0,
		"lowerdir=/,upperdir=/mnt/upper,workdir=/mnt/work"); err != nil {
		fatal("mount overlay on /mnt/merged: %v", err)
	}

	// Move virtual mounts into the new root.
	// The overlay's /mnt/merged already has the lower layer's dirs,
	// but we need real mountpoints for proc/sys/dev.
	// On the overlay, mkdir is a copy-up — this creates them in upperdir.
	os.MkdirAll("/mnt/merged/proc", 0755)
	os.MkdirAll("/mnt/merged/sys", 0755)
	os.MkdirAll("/mnt/merged/dev", 0755)

	// Use MS_MOVE to migrate mounts into the merged root.
	// If MS_MOVE fails (e.g. mount is busy or shared), fall back to fresh mount.
	if err := syscall.Mount("/proc", "/mnt/merged/proc", "", syscall.MS_MOVE, ""); err != nil {
		logMsg("MS_MOVE /proc failed (%v), remounting fresh", err)
		syscall.Mount("proc", "/mnt/merged/proc", "proc", 0, "")
	}
	if err := syscall.Mount("/sys", "/mnt/merged/sys", "", syscall.MS_MOVE, ""); err != nil {
		logMsg("MS_MOVE /sys failed (%v), remounting fresh", err)
		syscall.Mount("sysfs", "/mnt/merged/sys", "sysfs", 0, "")
	}
	if err := syscall.Mount("/dev", "/mnt/merged/dev", "", syscall.MS_MOVE, ""); err != nil {
		logMsg("MS_MOVE /dev failed (%v), remounting fresh", err)
		syscall.Mount("devtmpfs", "/mnt/merged/dev", "devtmpfs", 0, "")
	}

	// Ensure /tmp is writable
	os.MkdirAll("/mnt/merged/tmp", 01777)
	os.Chmod("/mnt/merged/tmp", 01777)

	// Re-open console on the merged dev before pivot_root
	if logFile != nil {
		logFile.Close()
		logFile = nil
	}
	for _, path := range []string{"/mnt/merged/dev/console", "/mnt/merged/dev/ttyS0"} {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err == nil {
			logFile = f
			break
		}
	}

	// pivot_root: new root = /mnt/merged, old root goes to /mnt/merged/rom
	os.MkdirAll("/mnt/merged/rom", 0755)
	if err := os.Chdir("/mnt/merged"); err != nil {
		fatal("chdir /mnt/merged: %v", err)
	}
	if err := syscall.PivotRoot(".", "rom"); err != nil {
		fatal("pivot_root: %v", err)
	}
	if err := os.Chdir("/"); err != nil {
		fatal("chdir /: %v", err)
	}

	// Re-open console after pivot (fd paths changed)
	if logFile != nil {
		logFile.Close()
		logFile = nil
	}
	initConsole()

	// Bring up loopback (needed for localhost health checks)
	bringUpLoopback()

	// Write a working /etc/resolv.conf so functions can resolve DNS names.
	setupDNS()
}

func setupDNS() {
	const resolvConf = "nameserver 8.8.8.8\nnameserver 8.8.4.4\n"
	os.MkdirAll("/etc", 0755)
	if err := os.WriteFile("/etc/resolv.conf", []byte(resolvConf), 0644); err != nil {
		logMsg("warning: failed to write /etc/resolv.conf: %v", err)
	}
}

// bringUpLoopback brings up the lo interface using raw ioctl.
func bringUpLoopback() {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return
	}
	defer syscall.Close(fd)

	// struct ifreq for ioctl — name + flags
	type ifreq struct {
		name  [syscall.IFNAMSIZ]byte
		flags int16
		_pad  [22]byte
	}

	var req ifreq
	copy(req.name[:], "lo")

	// SIOCGIFFLAGS — get current flags
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&req))); errno != 0 {
		return
	}

	// Set IFF_UP
	req.flags |= syscall.IFF_UP

	// SIOCSIFFLAGS — apply
	syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&req)))
}

func main() {
	// Phase 1: Set up overlay and pivot_root
	setupOverlay()

	startClockSync()

	// Phase 2: Read entrypoint and launch function

	// Read entrypoint metadata
	data, err := os.ReadFile(entrypointPath)
	if err != nil {
		fatal("cannot read %s: %v", entrypointPath, err)
	}

	var ep fcEntrypoint
	if err := json.Unmarshal(data, &ep); err != nil {
		fatal("cannot parse %s: %v", entrypointPath, err)
	}

	if len(ep.Cmd) == 0 {
		fatal("no command specified in %s", entrypointPath)
	}

	// Apply environment variables from OCI config.
	// Always include a sane PATH fallback.
	env := make([]string, 0, len(ep.Env)+2)
	hasPath := false
	for _, e := range ep.Env {
		env = append(env, e)
		if strings.HasPrefix(e, "PATH=") {
			hasPath = true
		}
	}
	if !hasPath {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	if hostname, err := os.Hostname(); err == nil {
		env = append(env, "HOSTNAME="+hostname)
	}

	// Change working directory
	workdir := ep.Workdir
	if workdir == "" {
		workdir = "/"
	}
	if err := os.Chdir(workdir); err != nil {
		fmt.Fprintf(os.Stderr, "fc-launcher: warning: chdir %s: %v (falling back to /)\n", workdir, err)
		os.Chdir("/")
	}

	// Resolve executable
	binary := ep.Cmd[0]
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			os.Setenv("PATH", strings.TrimPrefix(e, "PATH="))
			break
		}
	}
	resolvedBinary, err := exec.LookPath(binary)
	if err != nil {
		fatal("executable not found: %s: %v", binary, err)
	}

	cmd := &exec.Cmd{
		Path:   resolvedBinary,
		Args:   ep.Cmd,
		Env:    env,
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		SysProcAttr: &syscall.SysProcAttr{
			Setpgid: true,
		},
	}

	if err := cmd.Start(); err != nil {
		fatal("failed to start %v: %v", ep.Cmd, err)
	}

	childPID := cmd.Process.Pid

	// Signal forwarding to child process group
	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh,
		syscall.SIGTERM,
		syscall.SIGINT,
		syscall.SIGHUP,
		syscall.SIGQUIT,
		syscall.SIGUSR1,
		syscall.SIGUSR2,
	)

	go func() {
		for sig := range sigCh {
			syscall.Kill(-childPID, sig.(syscall.Signal))
		}
	}()

	exitCode := 0
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err != nil {
			if err == syscall.ECHILD {
				break
			}
			continue
		}

		if pid == childPID {
			if ws.Exited() {
				exitCode = ws.ExitStatus()
			} else if ws.Signaled() {
				exitCode = 128 + int(ws.Signal())
			}
			break
		}
	}

	os.Exit(exitCode)
}

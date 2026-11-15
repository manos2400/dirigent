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

package main

import (
	"os"
	"syscall"
	"time"
	"unsafe"
)

// A guest restored from a snapshot resumes with the clock it had when the
// snapshot was taken, and nothing in the guest ever corrects it, so its offset
// from real time stays fixed at (restore time - snapshot time) for the rest of
// its life. Functions that sign object-storage requests are rejected once that
// offset passes the signature skew tolerance, which is 900 s for S3 and MinIO.
//
// ptp_kvm exposes the host clock to the guest as a PTP device, so the offset can
// be read and stepped away. This loop runs in PID 1, which means it is captured
// in the snapshot and resumes with the guest.
const (
	ptpDevice = "/dev/ptp0"

	// Poll often enough that the correction lands before the first invocation
	// reaches a restored guest. The cost is one hypercall per tick.
	clockSyncInterval = 250 * time.Millisecond

	// Step only on a real jump. Ordinary drift under kvm-clock stays far below
	// this, so a healthy guest never touches its clock.
	clockSyncThreshold = 250 * time.Millisecond
)

// fdToClockID mirrors the kernel's FD_TO_CLOCKID: a dynamic POSIX clock is
// addressed by a negative clockid derived from the file descriptor.
func fdToClockID(fd int) uintptr {
	return uintptr(int64((^int32(fd) << 3) | 3))
}

func clockGettime(clockID uintptr) (syscall.Timespec, error) {
	var ts syscall.Timespec

	if _, _, errno := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, clockID,
		uintptr(unsafe.Pointer(&ts)), 0); errno != 0 {
		return ts, errno
	}

	return ts, nil
}

func clockSettime(clockID uintptr, ts syscall.Timespec) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_CLOCK_SETTIME, clockID,
		uintptr(unsafe.Pointer(&ts)), 0); errno != 0 {
		return errno
	}

	return nil
}

func startClockSync() {
	f, err := os.Open(ptpDevice)
	if err != nil {
		logMsg("fc-launcher: clock sync disabled: cannot open %s: %v (guests restored from snapshots will run behind real time)", ptpDevice, err)
		return
	}

	hostClock := fdToClockID(int(f.Fd()))

	if _, err := clockGettime(hostClock); err != nil {
		logMsg("fc-launcher: clock sync disabled: %s is not readable as a clock: %v", ptpDevice, err)
		f.Close()
		return
	}

	go func() {
		defer f.Close()

		ticker := time.NewTicker(clockSyncInterval)
		defer ticker.Stop()

		for range ticker.C {
			host, err := clockGettime(hostClock)
			if err != nil {
				continue
			}

			guest, err := clockGettime(uintptr(0)) // CLOCK_REALTIME
			if err != nil {
				continue
			}

			offset := time.Duration(host.Nano() - guest.Nano())
			if offset < clockSyncThreshold && offset > -clockSyncThreshold {
				continue
			}

			if err := clockSettime(uintptr(0), host); err != nil {
				logMsg("fc-launcher: failed to step the clock by %v: %v", offset, err)
				continue
			}

			logMsg("fc-launcher: stepped the guest clock by %v", offset)
		}
	}()
}

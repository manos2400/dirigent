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

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/sirupsen/logrus"
)

type VMControlStructure struct {
	Context context.Context

	VM                   *firecracker.Machine
	VMConfig             *firecracker.Config
	NetworkConfiguration *NetworkConfig

	SandboxID string
	HostPID   int

	KernelPath     string
	FileSystemPath string

	RequestedCPUCores    uint64
	RequestedMemoryMiB   uint64
	EffectiveVMMemoryMiB uint64

	HostPort  int
	GuestPort int
}

// sandboxCgroupFD, when >= 0, is a directory fd of the sandbox cgroup. Each
// Firecracker process is cloned directly into it (CLONE_INTO_CGROUP) so all
// guest memory is charged to the sandbox cgroup from the first instruction,
// instead of leaking into the worker's cgroup. Set via SetSandboxCgroupFD.
var sandboxCgroupFD = -1

// SetSandboxCgroupFD enables placing Firecracker VMs into the given cgroup fd.
func SetSandboxCgroupFD(fd int) { sandboxCgroupFD = fd }

func getVMCommandBuild(vmcs *VMControlStructure) *exec.Cmd {
	vmCommandBuild := firecracker.VMCommandBuilder{}.
		WithBin("firecracker").
		WithSocketPath(vmcs.VMConfig.SocketPath)

	if logrus.GetLevel() != logrus.InfoLevel {
		vmCommandBuild = vmCommandBuild.
			WithStdin(os.Stdin).
			WithStdout(os.Stdout).
			WithStderr(os.Stderr)
	}

	cmd := vmCommandBuild.Build(vmcs.Context)

	if sandboxCgroupFD >= 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			UseCgroupFD: true,
			CgroupFD:    sandboxCgroupFD,
		}
	}

	return cmd
}

func StartFirecrackerVM(networkManager *NetworkPoolManager, vmcs *VMControlStructure, vmDebugMode bool, snapshotMetadata *SnapshotMetadata) (error, time.Duration, time.Duration, time.Duration) {
	networkCreation := time.Now()
	vmcs.NetworkConfiguration = networkManager.GetOneConfig()
	if vmcs.NetworkConfiguration == nil {
		return errors.New("error getting a network interface"), time.Duration(0), time.Duration(0), time.Duration(0)
	}
	tapEnd := time.Since(networkCreation)
	logrus.Debug("Time to create network: ", tapEnd.Milliseconds(), " ms")

	logrus.Debugf("VM %s TAP external = %s, TAP internal = %s, TAP MAC = %s, VETH external (%s) = %s, VETH internal = %s",
		vmcs.SandboxID,
		vmcs.NetworkConfiguration.TapExternalIP,
		vmcs.NetworkConfiguration.TapInternalIP,
		vmcs.NetworkConfiguration.TapMAC,
		vmcs.NetworkConfiguration.VETHHostName,
		vmcs.NetworkConfiguration.VETHExternalIP,
		vmcs.NetworkConfiguration.VETHInternalIP,
	)

	startVMCreation := time.Now()

	makeFirecrackerConfig(vmcs, vmDebugMode, snapshotMetadata)
	newMachineOpts := []firecracker.Opt{firecracker.WithProcessRunner(getVMCommandBuild(vmcs))}
	if vmDebugMode {
		logger := logrus.New()
		logger.SetLevel(logrus.GetLevel())
		newMachineOpts = append(newMachineOpts, firecracker.WithLogger(logrus.NewEntry(logger)))
	}

	if snapshotMetadata != nil {
		snapshotsOpts := firecracker.WithSnapshot(snapshotMetadata.MemoryPath, snapshotMetadata.SnapshotPath)
		newMachineOpts = append(newMachineOpts, snapshotsOpts)

		vmcs.VMConfig = &firecracker.Config{
			SocketPath:        vmcs.VMConfig.SocketPath,
			LogPath:           vmcs.VMConfig.LogPath,
			LogLevel:          vmcs.VMConfig.LogLevel,
			Drives:            vmcs.VMConfig.Drives,
			NetworkInterfaces: vmcs.VMConfig.NetworkInterfaces,
			NetNS:             vmcs.VMConfig.NetNS,
		}
	}

	machine, err := firecracker.NewMachine(vmcs.Context, *vmcs.VMConfig, newMachineOpts...)
	if err != nil {
		logrus.Errorf("Failed creating a new virtual machine - %v", err)
		networkManager.GiveUpNetwork(vmcs.NetworkConfiguration)

		return err, tapEnd, time.Since(startVMCreation), time.Duration(0)
	}
	vmcs.VM = machine

	vmCreateEnd := time.Since(startVMCreation)
	logrus.Debug("VM creation time: ", vmCreateEnd.Milliseconds(), " ms")

	timeVMStart := time.Now()

	err = machine.Start(vmcs.Context)
	if err != nil {
		logrus.Errorf("Error starting a virtual machine - %v", err)
		networkManager.GiveUpNetwork(vmcs.NetworkConfiguration)

		return err, tapEnd, vmCreateEnd, time.Since(timeVMStart)
	}

	if snapshotMetadata != nil {
		err = machine.ResumeVM(vmcs.Context)
		if err != nil {
			logrus.Errorf("Error creating virtual machine from snapshot - %v", err)
			networkManager.GiveUpNetwork(vmcs.NetworkConfiguration)

			return err, tapEnd, vmCreateEnd, time.Since(timeVMStart)
		}
		refreshNetworkAfterResume(vmcs.NetworkConfiguration)
	}

	vmStartEnd := time.Since(timeVMStart)
	logrus.Debug("VM starting time: ", vmStartEnd.Milliseconds(), " ms")

	return err, tapEnd, vmCreateEnd, vmStartEnd
}

func StopFirecrackerVM(vmcs *VMControlStructure) error {
	pid, err := vmcs.VM.PID()
	if err != nil {
		return err
	}

	err = syscall.Kill(pid, syscall.SIGKILL)
	return err
}

func deleteLogs(vmcs *VMControlStructure) {
	// don't handle error because the file may not exist
	_ = os.Remove(fmt.Sprintf(pathToLog, vmcs.SandboxID))
}

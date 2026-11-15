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
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"
)

const (
	amd64NoDebugKernelArgs = "ro init=/sbin/fc-launcher panic=1 pci=off nomodule reboot=k tsc=reliable quiet i8042.nokbd i8042.noaux 8250.nr_uarts=0 ipv6.disable=1"
	amd64DebugKernelArgs   = "ro init=/sbin/fc-launcher panic=1 pci=off nomodule reboot=k tsc=reliable quiet i8042.noaux ipv6.disable=1 console=ttyS0 random.trust_cpu=on"
	arm64NoDebugKernelArgs = "ro init=/sbin/fc-launcher panic=1 pci=off nomodule reboot=k quiet ipv6.disable=1 random.trust_cpu=on"
	arm64DebugKernelArgs   = "ro init=/sbin/fc-launcher panic=1 pci=off nomodule reboot=k quiet ipv6.disable=1 console=ttyS0 keep_bootcon random.trust_cpu=on"
	ipKernelArg            = " ip=%s::%s:255.255.255.252::eth0:off"
	pathToLog              = "/tmp/%s.log"
)

func guestKernelArgs(vmDebugMode bool) string {
	switch runtime.GOARCH {
	case "arm64":
		if vmDebugMode {
			return arm64DebugKernelArgs
		}

		return arm64NoDebugKernelArgs
	default:
		if vmDebugMode {
			return amd64DebugKernelArgs
		}

		return amd64NoDebugKernelArgs
	}
}

func makeSocketPath(vmmID string) string {
	return filepath.Join(os.TempDir(), vmmID)
}

func makeFirecrackerConfig(vmcs *VMControlStructure, vmDebugMode bool, metadata *SnapshotMetadata) {
	if vmcs.NetworkConfiguration == nil {
		logrus.Error("Network must be created before creating a Firecracker config.")
		return
	}

	kernelArgs := guestKernelArgs(vmDebugMode)
	kernelArgs += fmt.Sprintf(ipKernelArg, vmcs.NetworkConfiguration.TapInternalIP, vmcs.NetworkConfiguration.TapExternalIP)

	memoryMiB := vmcs.EffectiveVMMemoryMiB
	if memoryMiB > math.MaxInt64 {
		memoryMiB = math.MaxInt64
	}

	vcpus := vmcs.RequestedCPUCores
	if vcpus == 0 {
		vcpus = 1
	}
	if vcpus > math.MaxInt64 {
		vcpus = math.MaxInt64
	}

	vmcs.VMConfig = &firecracker.Config{
		SocketPath:      makeSocketPath(vmcs.SandboxID),
		KernelImagePath: vmcs.KernelPath,
		KernelArgs:      kernelArgs,
		LogPath:         fmt.Sprintf(pathToLog, vmcs.SandboxID),
		LogLevel:        "Info",
		Drives: []models.Drive{{
			DriveID:      firecracker.String("1"),
			PathOnHost:   firecracker.String(vmcs.FileSystemPath),
			IsReadOnly:   firecracker.Bool(true),
			IsRootDevice: firecracker.Bool(true),
		}},
		MachineCfg: models.MachineConfiguration{
			MemSizeMib: firecracker.Int64(int64(memoryMiB)),
			VcpuCount:  firecracker.Int64(int64(vcpus)),
			Smt:        firecracker.Bool(false),
		},
		NetworkInterfaces: []firecracker.NetworkInterface{{
			StaticConfiguration: &firecracker.StaticNetworkConfiguration{
				HostDevName: vmcs.NetworkConfiguration.TapDeviceName,
				MacAddress:  vmcs.NetworkConfiguration.TapMAC,
			},
		}},
		NetNS: fmt.Sprintf("/var/run/netns/%s", vmcs.NetworkConfiguration.NetNS),
	}
}

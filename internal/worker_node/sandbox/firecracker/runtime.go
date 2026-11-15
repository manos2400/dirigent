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
	"cluster_manager/api/proto"
	"cluster_manager/internal/worker_node/managers"
	"cluster_manager/internal/worker_node/sandbox"
	"cluster_manager/internal/worker_node/sandbox/containerd"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

const snapshotCreationTimeout = 5 * time.Minute

type Runtime struct {
	sandbox.RuntimeInterface

	cpApi       proto.CpiInterfaceClient
	idGenerator *managers.ThreadSafeRandomGenerator

	VMDebugMode  bool
	UseSnapshots bool

	KernelPath     string
	RootfsBuilder  *RootfsBuilder
	NetworkManager *NetworkPoolManager

	SandboxManager  *managers.SandboxManager
	ProcessMonitor  *managers.ProcessMonitor
	SnapshotManager *SnapshotManager
	IPT             *iptables.IPTables
}

type FirecrackerMetadata struct {
	managers.RuntimeMetadata

	VMCS *VMControlStructure
}

func NewFirecrackerRuntime(cpApi proto.CpiInterfaceClient, sandboxManager *managers.SandboxManager,
	kernelPath string, launcherPath string, imageStorePath string, internalIPPrefix string,
	vmDebugMode bool, useSnapshots bool, networkPoolSize int) *Runtime {

	DeleteAllSnapshots()
	err := DeleteUnusedNetworkDevices()
	if err != nil {
		logrus.Error("Failed to remove some or all network devices.")
	}

	ipt, err := managers.NewIptablesUtil()
	if err != nil {
		logrus.Fatal("Failed to start iptables utility. Terminating worker daemon.")
	}

	rootfsBuilder, err := NewRootfsBuilder(launcherPath, imageStorePath)
	if err != nil {
		logrus.Fatalf("Failed to create rootfs builder: %v", err)
	}

	return &Runtime{
		cpApi:       cpApi,
		idGenerator: managers.NewThreadSafeRandomGenerator(),

		VMDebugMode:  vmDebugMode,
		UseSnapshots: useSnapshots,

		KernelPath:     kernelPath,
		RootfsBuilder:  rootfsBuilder,
		NetworkManager: NewNetworkPoolManager(internalIPPrefix, networkPoolSize),

		SandboxManager:  sandboxManager,
		ProcessMonitor:  managers.NewProcessMonitor(),
		SnapshotManager: NewFirecrackerSnapshotManager(),
		IPT:             ipt,
	}
}

func (fcr *Runtime) createVMCS(rootfsPath string, in *proto.ServiceInfo) *VMControlStructure {
	requestedCPUCores := in.RequestedCpu
	if requestedCPUCores == 0 {
		requestedCPUCores = 1
	}

	effectiveMemoryMiB := computeEffectiveVMMemoryMiB(in.RequestedMemory)

	return &VMControlStructure{
		Context: context.Background(),

		KernelPath:           fcr.KernelPath,
		FileSystemPath:       rootfsPath,
		RequestedCPUCores:    requestedCPUCores,
		RequestedMemoryMiB:   in.RequestedMemory,
		EffectiveVMMemoryMiB: effectiveMemoryMiB,

		SandboxID: fmt.Sprintf("firecracker-%d", fcr.idGenerator.Int()),
	}
}

func createMetadata(in *proto.ServiceInfo, vmcs *VMControlStructure) *managers.Metadata {
	return &managers.Metadata{
		ServiceName: in.Name,

		RuntimeMetadata: FirecrackerMetadata{
			VMCS: vmcs,
		},

		IP:        vmcs.NetworkConfiguration.TapInternalIP,
		GuestPort: int(in.PortForwarding.GuestPort),

		// Buffered so the process monitor can hand off an exit code even if the
		// watcher has not started reading yet.
		ExitStatusChannel: make(chan uint32, 1),
	}
}

func (fcr *Runtime) CreateSandbox(ctx context.Context, in *proto.ServiceInfo) (*proto.SandboxCreationStatus, error) {
	start := time.Now()

	// Build (or retrieve from cache) the per-function rootfs
	startImageFetch := time.Now()
	functionRootfs, err := fcr.RootfsBuilder.GetOrBuild(in.Image)
	if err != nil {
		logrus.Errorf("Failed to build rootfs for image %s: %v", in.Image, err)
		return &proto.SandboxCreationStatus{Success: false}, err
	}
	imageFetchDuration := time.Since(startImageFetch)

	vmcs := fcr.createVMCS(functionRootfs.RootfsPath, in)
	snapshotKey := SnapshotKey(in.Image, vmcs.EffectiveVMMemoryMiB, vmcs.RequestedCPUCores, in.PortForwarding.GuestPort)

	startFindSnapshot := time.Now()
	snapshot, _ := fcr.SnapshotManager.FindSnapshot(snapshotKey)
	if snapshot != nil {
		logrus.Infof("Snapshot found for image %s (mem=%dMiB, cpu=%d)", in.Image, vmcs.EffectiveVMMemoryMiB, vmcs.RequestedCPUCores)
	}
	findSnapshotDuration := time.Since(startFindSnapshot)

	err, netCreateDuration, vmCreateDuration, vmStartDuration := StartFirecrackerVM(fcr.NetworkManager, vmcs, fcr.VMDebugMode, snapshot)
	if err != nil {
		// resource deallocation already done in StartFirecrackerVM
		return &proto.SandboxCreationStatus{Success: false}, err
	}

	startConfigureMonitoring := time.Now()
	metadata := createMetadata(in, vmcs)
	metadata.HostPort = containerd.AssignRandomPort()
	metadata.IP = vmcs.NetworkConfiguration.VETHExternalIP

	// VM process monitoring
	vmPID, err := vmcs.VM.PID()
	if err != nil {
		fcr.NetworkManager.GiveUpNetwork(vmcs.NetworkConfiguration)
		containerd.UnassignPort(metadata.HostPort)

		logrus.Debugf("Failed to get PID of the virtual machine - %v", err)
		return &proto.SandboxCreationStatus{Success: false}, err
	}
	vmcs.HostPID = vmPID

	fcr.SandboxManager.AddSandbox(vmcs.SandboxID, metadata)

	go containerd.WatchExitChannel(fcr.cpApi, metadata, func(metadata *managers.Metadata) string {
		return metadata.RuntimeMetadata.(FirecrackerMetadata).VMCS.SandboxID
	})
	fcr.ProcessMonitor.AddChannel(uint32(vmPID), metadata.ExitStatusChannel)
	configureMonitoringDuration := time.Since(startConfigureMonitoring)

	// port forwarding
	iptablesStart := time.Now()
	managers.AddRules(fcr.IPT, metadata.HostPort, metadata.IP, metadata.GuestPort)
	iptablesDuration := time.Since(iptablesStart)

	in.PortForwarding.HostPort = int32(metadata.HostPort)

	logrus.Debug("Worker node part: ", time.Since(start).Milliseconds(), " ms")

	// blocking call
	timeToPass, passed := managers.SendReadinessProbe(fmt.Sprintf("localhost:%d", metadata.HostPort))

	// Create a snapshot for the service if it does not exist. Only one sandbox per snapshot key
	// does this: the others are already booted, and snapshotting would pause them for nothing.
	startSnapshotCreation := time.Now()
	if passed && fcr.UseSnapshots && fcr.SnapshotManager.TryClaimCreation(snapshotKey) {
		snapshot, vmUsable := fcr.captureSnapshot(vmcs, snapshotKey)
		if snapshot == nil {
			logrus.Warn("Due to failure, bypassing snapshot creation for image ", in.Image)
		} else {
			logrus.Debug("Snapshot successfully created for image ", in.Image)
		}

		passed = vmUsable // a guest that could not be resumed is dead; let deletion below handle it
	}
	snapshotCreationDuration := time.Since(startSnapshotCreation)

	if passed {
		return &proto.SandboxCreationStatus{
			Success:      true,
			ID:           vmcs.SandboxID,
			PortMappings: in.PortForwarding,
			LatencyBreakdown: &proto.SandboxCreationBreakdown{
				Total:               durationpb.New(time.Since(start)),
				ImageFetch:          durationpb.New(imageFetchDuration),
				SandboxCreate:       durationpb.New(vmCreateDuration),
				NetworkSetup:        durationpb.New(netCreateDuration),
				SandboxStart:        durationpb.New(vmStartDuration),
				ReadinessProbing:    durationpb.New(timeToPass),
				Iptables:            durationpb.New(iptablesDuration),
				SnapshotCreation:    durationpb.New(snapshotCreationDuration),
				ConfigureMonitoring: durationpb.New(configureMonitoringDuration),
				FindSnapshot:        durationpb.New(findSnapshotDuration),
			},
		}, nil
	} else {
		_, _ = fcr.DeleteSandbox(context.Background(), &proto.SandboxID{ID: vmcs.SandboxID})

		return &proto.SandboxCreationStatus{
			Success: false,
			ID:      vmcs.SandboxID,
		}, nil
	}
}

func (fcr *Runtime) captureSnapshot(vmcs *VMControlStructure, snapshotKey string) (*SnapshotMetadata, bool) {
	defer fcr.SnapshotManager.ReleaseCreation(snapshotKey)

	ctx, cancel := context.WithTimeout(context.Background(), snapshotCreationTimeout)
	defer cancel()

	snapshot, vmUsable := CreateVMSnapshot(ctx, vmcs)
	if snapshot == nil {
		fcr.SnapshotManager.RecordFailure(snapshotKey)
		return nil, vmUsable
	}

	fcr.SnapshotManager.AddSnapshot(snapshotKey, snapshot)

	return snapshot, vmUsable
}

func CreateVMSnapshot(ctx context.Context, vmcs *VMControlStructure) (*SnapshotMetadata, bool) {
	if err := vmcs.VM.PauseVM(ctx); err != nil {
		logrus.Errorf("Error pausing virtual machine %s - %v", vmcs.SandboxID, err)
		return nil, true
	}

	snapshot, err := dumpVMSnapshot(ctx, vmcs)
	if err != nil {
		logrus.Errorf("Error creating a snapshot out of virtual machine %s - %v", vmcs.SandboxID, err)
	}

	if err := vmcs.VM.ResumeVM(ctx); err != nil {
		logrus.Errorf("Error resuming virtual machine %s after snapshotting - %v", vmcs.SandboxID, err)
		return nil, false
	}
	refreshNetworkAfterResume(vmcs.NetworkConfiguration)

	return snapshot, true
}

func dumpVMSnapshot(ctx context.Context, vmcs *VMControlStructure) (*SnapshotMetadata, error) {
	snapshotDir := snapshotDirectory()
	if err := os.MkdirAll(snapshotDir, os.ModePerm); err != nil {
		return nil, fmt.Errorf("creating snapshot directory %s: %w", snapshotDir, err)
	}

	snapshot := &SnapshotMetadata{
		MemoryPath:   filepath.Join(snapshotDir, fmt.Sprintf("memory-%s", vmcs.SandboxID)),
		SnapshotPath: filepath.Join(snapshotDir, fmt.Sprintf("snapshot-%s", vmcs.SandboxID)),
	}

	if err := vmcs.VM.CreateSnapshot(ctx, snapshot.MemoryPath, snapshot.SnapshotPath); err != nil {
		// remove partial dump
		_ = os.Remove(snapshot.MemoryPath)
		_ = os.Remove(snapshot.SnapshotPath)

		return nil, fmt.Errorf("dumping snapshot: %w", err)
	}

	return snapshot, nil
}

func (fcr *Runtime) DeleteSandbox(_ context.Context, in *proto.SandboxID) (*proto.ActionStatus, error) {
	metadata := fcr.SandboxManager.DeleteSandbox(in.ID)
	if metadata == nil {
		logrus.Errorf("Error while deleting sandbox from the manager. Invalid name %s.", in.ID)
		return &proto.ActionStatus{Success: false}, nil
	}

	start := time.Now()

	sandboxMetadata := metadata.RuntimeMetadata.(FirecrackerMetadata)

	// delete networking rules
	managers.DeleteRules(fcr.IPT, metadata.HostPort, metadata.IP, metadata.GuestPort)
	containerd.UnassignPort(metadata.HostPort)
	logrus.Debug("IP tables configuration (remove rule(s)) took ", time.Since(start).Microseconds(), " μs")

	// destroy the VM
	err := StopFirecrackerVM(sandboxMetadata.VMCS)
	if err != nil {
		logrus.Errorf("Error deleting a sandbox - %v", err)
	}

	fcr.ProcessMonitor.RemoveChannel(uint32(sandboxMetadata.VMCS.HostPID))

	// delete the network associated with the VM
	fcr.NetworkManager.GiveUpNetwork(sandboxMetadata.VMCS.NetworkConfiguration)
	// delete Firecracker logs
	deleteLogs(sandboxMetadata.VMCS)

	logrus.Debug("Sandbox deletion took ", time.Since(start).Microseconds(), " μs")
	return &proto.ActionStatus{Success: true}, nil
}

func (fcr *Runtime) ListEndpoints(_ context.Context, _ *emptypb.Empty) (*proto.EndpointsList, error) {
	return fcr.SandboxManager.ListEndpoints()
}

func (fcr *Runtime) ValidateHostConfig() bool {
	// Check for KVM access
	err := unix.Access("/dev/kvm", unix.W_OK)
	if err != nil {
		logrus.Errorf("KVM access denied - %v", err)
		return false
	}

	return true
}

func snapshotDirectory() string {
	return filepath.Join(os.TempDir(), "snapshots")
}

func DeleteAllSnapshots() {
	// don't handle error because the directory may not exist
	_ = os.RemoveAll(snapshotDirectory())
}

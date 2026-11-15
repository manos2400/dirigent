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
	"cluster_manager/internal/worker_node/managers"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

const (
	vethPrefix                    = "fc-"
	vethSuffix                    = "-veth"
	masqChainName                 = "FC_MASQ"
	forwardChain                  = "FC_FWD"
	maxConsecutiveNetworkFailures = 16
)

var (
	arpingAvailable    bool
	conntrackAvailable bool
	snatRandomFully    bool
)

type NetworkConfig struct {
	NetNS string

	TapDeviceName string
	TapMAC        string
	TapExternalIP string
	TapInternalIP string

	VETHHostName   string
	VETHInternalIP string
	VETHExternalIP string
}

type NetworkPoolManager struct {
	sync.Mutex

	InternalIPManager *managers.IPManager

	InterfaceNameGenerator *managers.VETHNameGenerator
	NetNSNameGenerator     *managers.ThreadSafeRandomGenerator

	MaxNetworkPoolSize int

	pool      []*NetworkConfig
	refilling bool
}

func NewNetworkPoolManager(internalPrefix string, networkPoolSize int) *NetworkPoolManager {
	internalIPManager := managers.NewIPManager(internalPrefix)

	// NetworkPoolSize should be divisible by 16
	networkPoolSize -= (networkPoolSize % 16)
	if networkPoolSize < 16 {
		networkPoolSize = 16
	}

	pool := &NetworkPoolManager{
		InternalIPManager: internalIPManager,

		InterfaceNameGenerator: managers.NewVETHNameGenerator(),
		NetNSNameGenerator:     managers.NewThreadSafeRandomGenerator(),

		MaxNetworkPoolSize: networkPoolSize,
	}

	if _, err := exec.LookPath("arping"); err != nil {
		logrus.Warnf("arping not found in PATH: %v (snapshot restores will only flush the neighbour cache)", err)
	} else {
		arpingAvailable = true
	}

	if _, err := exec.LookPath("conntrack"); err != nil {
		logrus.Warnf("conntrack not found in PATH: %v (recycled namespaces will keep the previous guest's flow state, which breaks outbound connections from restored guests)", err)
	} else {
		conntrackAvailable = true
	}

	snatRandomFully = probeRandomFully()

	setupForwardChain(internalPrefix)
	setupMasqueradeChain(internalPrefix)
	pool.populate()

	return pool
}

func (np *NetworkPoolManager) GiveUpNetwork(config *NetworkConfig) {
	// Reset before the config becomes visible to another sandbox, so a namespace
	// in the pool is always clean.
	resetNetworkState(config)

	recycled := false

	np.Lock()
	if len(np.pool) < np.MaxNetworkPoolSize {
		np.pool = append(np.pool, config)
		recycled = true
	}
	np.Unlock()

	if !recycled {
		deleteNetworkNamespaceByName(config.NetNS)
		deleteDeviceByName(config.VETHHostName)
	}
}

// resetNetworkState drops the flow state a departing guest left behind in a
// namespace that is about to be reused.
func resetNetworkState(cfg *NetworkConfig) {
	if conntrackAvailable {
		// Conntrack is per-namespace, so this only touches flows of the guest that
		// just went away. Exit status 1 means the table was already empty.
		cmd := exec.Command("sudo", "ip", "netns", "exec", cfg.NetNS, "conntrack", "-F")
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				logrus.Warnf("network recycle: failed to flush conntrack in %s: %v", cfg.NetNS, err)
			}
		}
	}

	if err := exec.Command("sudo", "ip", "netns", "exec", cfg.NetNS,
		"ip", "neigh", "flush", "dev", cfg.TapDeviceName).Run(); err != nil {
		logrus.Warnf("network recycle: failed to flush neighbours on %s in %s: %v", cfg.TapDeviceName, cfg.NetNS, err)
	}
}

func probeRandomFully() bool {
	err := exec.Command("sudo", "iptables", "-t", "nat", "-C", "POSTROUTING",
		"-j", "SNAT", "--to-source", "127.0.0.1", "--random-fully").Run()

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// Rule absent (exit 1) still proves the flag parsed; a parse failure exits 2.
		return exitErr.ExitCode() == 1
	}

	if err != nil {
		logrus.Warnf("could not probe iptables --random-fully support: %v", err)
		return false
	}

	return true
}

func (np *NetworkPoolManager) populate() {
	failures := 0

	for {
		np.Lock()
		full := len(np.pool) >= np.MaxNetworkPoolSize
		np.Unlock()

		if full {
			return
		}
		config, err := np.createNetwork()
		if err != nil {
			logrus.Errorf("Error creating a network - %v", err)

			failures++
			if failures >= maxConsecutiveNetworkFailures {
				logrus.Errorf("Giving up on populating the network pool after %d consecutive failures.", failures)
				return
			}

			continue
		}

		failures = 0

		np.Lock()
		np.pool = append(np.pool, config)
		np.Unlock()
	}
}

// triggerRefill starts a background refill unless one is already running. The
// caller must hold the pool lock.
func (np *NetworkPoolManager) triggerRefill() {
	if np.refilling {
		return
	}
	np.refilling = true

	go func() {
		np.populate()

		np.Lock()
		np.refilling = false
		np.Unlock()
	}()
}

func (np *NetworkPoolManager) GetOneConfig() *NetworkConfig {
	np.Lock()

	if len(np.pool) == 0 {
		logrus.Warn("Network pool is empty. Creating a network on the critical path and repopulating.")
		np.triggerRefill()
		np.Unlock()

		config, err := np.createNetwork()
		if err != nil {
			logrus.Error("Error creating network on the critical path.")
			return nil
		}

		return config
	}

	// Refill from half full rather than on an exact count, so a burst that skips
	// past the threshold cannot leave the pool draining with no refill running.
	if len(np.pool) <= np.MaxNetworkPoolSize/2 {
		np.triggerRefill()
	}

	cfg := np.pool[0]
	np.pool = np.pool[1:]
	np.Unlock()

	return cfg
}

func (np *NetworkPoolManager) createNetwork() (*NetworkConfig, error) {
	// Networking for clones -- guide:
	// https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/network-for-clones.md

	// Hardcode each IP address. This is possible as they all reside in different network namespaces.
	gatewayIP, vmip, mac, dev := "169.254.0.1", "169.254.0.2", "02:FC:00:00:00:00", "fc-tap"

	config := &NetworkConfig{
		NetNS: fmt.Sprintf("firecracker-%d", np.NetNSNameGenerator.Int()),

		TapDeviceName: dev,
		TapMAC:        mac,
		TapExternalIP: gatewayIP,
		TapInternalIP: vmip,
	}

	err := exec.Command("sudo", "ip", "netns", "add", config.NetNS).Run()
	if err != nil {
		logrus.Errorf("failed to create network namespace %s - %v", config.NetNS, err)
		return nil, err
	}

	err = exec.Command("sudo", "ip", "netns", "exec", config.NetNS, "ip", "tuntap", "add", "dev", dev, "mode", "tap").Run()
	if err != nil {
		logrus.Errorf("failed to create a TUN/TAP device %s - %v", dev, err)
		deleteNetworkNamespaceByName(config.NetNS)
		return nil, err
	}

	err = exec.Command("sudo", "ip", "netns", "exec", config.NetNS, "ip", "addr", "add", gatewayIP+"/30", "dev", dev).Run()
	if err != nil {
		logrus.Errorf("failed to assign IP address to device %s - %v", dev, err)
		deleteNetworkNamespaceByName(config.NetNS)
		return nil, err
	}

	err = exec.Command("sudo", "ip", "netns", "exec", config.NetNS, "ip", "link", "set", "dev", dev, "up").Run()
	if err != nil {
		logrus.Errorf("failed to bring up the device %s - %v", dev, err)
		deleteNetworkNamespaceByName(config.NetNS)
		return nil, err
	}

	err = exec.Command("sudo", "ip", "netns", "exec", config.NetNS, "sysctl", "-w", "net.ipv4.ip_forward=1").Run()
	if err != nil {
		logrus.Warnf("failed to enable ip_forward in namespace %s - %v (outbound internet may not work)", config.NetNS, err)
	}

	vethInternalIP, vethExternalIP, err := np.createVETHPair(config, vmip)
	if err != nil {
		logrus.Errorf("failed to create and configure veth pair - %v", err)
		// network removal done inside the called function
		return nil, err
	}

	config.VETHInternalIP = vethInternalIP
	config.VETHExternalIP = vethExternalIP

	return config, nil
}

func (np *NetworkPoolManager) createVETHPair(config *NetworkConfig, guestTAP string) (string, string, error) {
	id := np.InterfaceNameGenerator.Generate()

	guestSidePairName := fmt.Sprintf("%s%d%s", vethPrefix, id, vethSuffix)
	hostSidePairName := fmt.Sprintf("%s%d%s", vethPrefix, id+1, vethSuffix)

	config.VETHHostName = hostSidePairName

	hostSideIpAddress, workloadIpAddress := np.InternalIPManager.GenerateIPMACPair()
	hostSideIpAddressWithMask := hostSideIpAddress + "/24"
	workloadIpAddressWithMask := workloadIpAddress + "/24"

	// create a vETH pair - both created in the netNS namespace
	err := exec.Command("sudo", "ip", "netns", "exec", config.NetNS, "ip", "link", "add", hostSidePairName, "type", "veth", "peer", "name", guestSidePairName).Run()
	if err != nil {
		logrus.Error("Error creating a virtual Ethernet pair - ", err)
		deleteNetworkNamespaceByName(config.NetNS)
		return hostSideIpAddress, workloadIpAddress, err
	}

	// move the host-side of te pair to the root namespace
	err = exec.Command("sudo", "ip", "netns", "exec", config.NetNS, "ip", "link", "set", hostSidePairName, "netns", "1").Run()
	if err != nil {
		logrus.Error("Error moving host-side of the pair to the root namespace - ", err)
		deleteNetworkNamespaceByName(config.NetNS)
		return hostSideIpAddress, workloadIpAddress, err
	}

	// assign an IP address to the guest-side pair
	err = exec.Command("sudo", "ip", "netns", "exec", config.NetNS, "ip", "addr", "add", workloadIpAddressWithMask, "dev", guestSidePairName).Run()
	if err != nil {
		logrus.Error("Error assigning an IP address to the guest-side of the pair - ", err)
		deleteNetworkNamespaceByName(config.NetNS)
		return hostSideIpAddress, workloadIpAddress, err
	}

	// bringing the guest-side pair up
	err = exec.Command("sudo", "ip", "netns", "exec", config.NetNS, "ip", "link", "set", "dev", guestSidePairName, "up").Run()
	if err != nil {
		logrus.Error("Error bringing the guest-end of the pair up - ", err)
		deleteNetworkNamespaceByName(config.NetNS)
		return hostSideIpAddress, workloadIpAddress, err
	}

	// assigning an IP address to the host-side of the pair
	err = exec.Command("sudo", "ip", "addr", "add", hostSideIpAddressWithMask, "dev", hostSidePairName).Run()
	if err != nil {
		logrus.Error("Error assigning an IP address to the host-end of the pair - ", err)
		deleteNetworkNamespaceByName(config.NetNS)
		deleteDeviceByName(hostSidePairName)
		return hostSideIpAddress, workloadIpAddress, err
	}

	// bringing the host-side pair up
	err = exec.Command("sudo", "ip", "link", "set", "dev", hostSidePairName, "up").Run()
	if err != nil {
		logrus.Error("Error bringing the host-end of the pair up - ", err)
		deleteNetworkNamespaceByName(config.NetNS)
		deleteDeviceByName(hostSidePairName)
		return hostSideIpAddress, workloadIpAddress, err
	}

	// assigning default gateway of the namespace
	err = exec.Command("sudo", "ip", "netns", "exec", config.NetNS, "ip", "route", "add", "default", "via", hostSideIpAddress).Run()
	if err != nil {
		logrus.Errorf("Error assigning default gateway of the namespace %s - %v", config.NetNS, err)
		deleteNetworkNamespaceByName(config.NetNS)
		deleteDeviceByName(hostSidePairName)
		return hostSideIpAddress, workloadIpAddress, err
	}

	snatArgs := []string{"ip", "netns", "exec", config.NetNS, "iptables", "-t", "nat",
		"-A", "POSTROUTING", "-o", guestSidePairName, "-s", guestTAP,
		"-j", "SNAT", "--to-source", workloadIpAddress}
	if snatRandomFully {
		snatArgs = append(snatArgs, "--random-fully")
	}

	err = exec.Command("sudo", snatArgs...).Run()
	if err != nil {
		logrus.Errorf("Error adding a NAT rule for the outgoing traffic from the namespace - %v", err)
		deleteNetworkNamespaceByName(config.NetNS)
		deleteDeviceByName(hostSidePairName)
		return hostSideIpAddress, workloadIpAddress, err
	}

	// Host-published ports are DNATed in the root namespace to the netns-side veth IP.
	// Translate them again inside the netns onto the guest tap address.
	err = exec.Command("sudo", "ip", "netns", "exec", config.NetNS, "iptables", "-t", "nat", "-A", "PREROUTING", "-i", guestSidePairName, "-d", workloadIpAddress, "-j", "DNAT", "--to-destination", guestTAP).Run()
	if err != nil {
		logrus.Errorf("Error adding a NAT rule for the incoming traffic to the namespace - %v", err)
		deleteNetworkNamespaceByName(config.NetNS)
		deleteDeviceByName(hostSidePairName)
		return hostSideIpAddress, workloadIpAddress, err
	}

	// Allow forwarded traffic inside the netns in both directions.
	err = exec.Command("sudo", "ip", "netns", "exec", config.NetNS, "iptables", "-A", "FORWARD", "-i", config.TapDeviceName, "-o", guestSidePairName, "-j", "ACCEPT").Run()
	if err != nil {
		logrus.Errorf("Error allowing VM egress forwarding inside the namespace - %v", err)
		deleteNetworkNamespaceByName(config.NetNS)
		deleteDeviceByName(hostSidePairName)
		return hostSideIpAddress, workloadIpAddress, err
	}

	err = exec.Command("sudo", "ip", "netns", "exec", config.NetNS, "iptables", "-A", "FORWARD", "-i", guestSidePairName, "-o", config.TapDeviceName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT").Run()
	if err != nil {
		logrus.Errorf("Error allowing VM return forwarding inside the namespace - %v", err)
		deleteNetworkNamespaceByName(config.NetNS)
		deleteDeviceByName(hostSidePairName)
		return hostSideIpAddress, workloadIpAddress, err
	}

	err = exec.Command("sudo", "ip", "netns", "exec", config.NetNS, "iptables", "-A", "FORWARD", "-i", guestSidePairName, "-o", config.TapDeviceName, "-d", guestTAP, "-j", "ACCEPT").Run()
	if err != nil {
		logrus.Errorf("Error allowing VM ingress forwarding inside the namespace - %v", err)
		deleteNetworkNamespaceByName(config.NetNS)
		deleteDeviceByName(hostSidePairName)
		return hostSideIpAddress, workloadIpAddress, err
	}

	err = exec.Command("sudo", "ip", "netns", "exec", config.NetNS, "iptables", "-A", "FORWARD", "-i", config.TapDeviceName, "-o", guestSidePairName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT").Run()
	if err != nil {
		logrus.Errorf("Error allowing VM ingress return forwarding inside the namespace - %v", err)
		deleteNetworkNamespaceByName(config.NetNS)
		deleteDeviceByName(hostSidePairName)
		return hostSideIpAddress, workloadIpAddress, err
	}

	return hostSideIpAddress, workloadIpAddress, nil
}

func DeleteUnusedNetworkDevices() error {
	devices, err := net.Interfaces()
	if err != nil {
		logrus.Errorf("Error listing network devices - %v.", err)
		return err
	}

	for _, dev := range devices {
		if !(strings.HasPrefix(dev.Name, vethPrefix) && strings.HasSuffix(dev.Name, vethSuffix)) {
			continue
		}

		deleteDeviceByName(dev.Name)
	}

	err = exec.Command("sudo", "ip", "-all", "netns", "delete").Run()
	if err != nil {
		logrus.Errorf("Error deleting network network namespaces - %v.", err)
	}

	teardownMasqueradeChain()
	teardownForwardChain()

	return err
}

func setupForwardChain(prefix string) {
	teardownForwardChain()

	subnet, err := subnetForPrefix(prefix)
	if err != nil {
		logrus.Warnf("Failed to derive Firecracker forwarding subnet from prefix %q: %v", prefix, err)
		return
	}

	if err := exec.Command("sudo", "iptables", "-N", forwardChain).Run(); err != nil {
		logrus.Warnf("Failed to create %s chain: %v (early VM egress may be blocked)", forwardChain, err)
		return
	}

	if err := exec.Command("sudo", "iptables", "-A", forwardChain, "-s", subnet, "-j", "ACCEPT").Run(); err != nil {
		logrus.Warnf("Failed to allow outbound forwarding for %s via %s: %v", subnet, forwardChain, err)
	}

	if err := exec.Command("sudo", "iptables", "-A", forwardChain, "-d", subnet, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT").Run(); err != nil {
		logrus.Warnf("Failed to allow return forwarding for %s via %s: %v", subnet, forwardChain, err)
	}

	if err := exec.Command("sudo", "iptables", "-I", "FORWARD", "1", "-j", forwardChain).Run(); err != nil {
		logrus.Warnf("Failed to jump to %s from FORWARD: %v", forwardChain, err)
	}

	logrus.Infof("Forwarding chain %s installed for %s", forwardChain, subnet)
}

func setupMasqueradeChain(prefix string) {
	// Clean up any previous chain first
	teardownMasqueradeChain()

	subnet, err := subnetForPrefix(prefix)
	if err != nil {
		logrus.Warnf("Failed to derive Firecracker masquerade subnet from prefix %q: %v", prefix, err)
		return
	}

	// Create the custom chain
	if err := exec.Command("sudo", "iptables", "-t", "nat", "-N", masqChainName).Run(); err != nil {
		logrus.Warnf("Failed to create %s chain: %v (outbound internet may not work)", masqChainName, err)
		return
	}

	// Keep guest-link and same-subnet traffic local. Everything else should
	// leave through the host uplink with the host source IP.
	for _, cidr := range []string{"169.254.0.0/16", subnet} {
		exec.Command("sudo", "iptables", "-t", "nat", "-A", masqChainName, "-d", cidr, "-j", "RETURN").Run()
	}

	// Everything else gets masqueraded
	if err := exec.Command("sudo", "iptables", "-t", "nat", "-A", masqChainName, "-j", "MASQUERADE").Run(); err != nil {
		logrus.Warnf("Failed to add MASQUERADE target to %s chain: %v", masqChainName, err)
	}

	// Jump from POSTROUTING into our chain for all VM traffic
	if err := exec.Command("sudo", "iptables", "-t", "nat", "-A", "POSTROUTING", "-s", subnet, "-j", masqChainName).Run(); err != nil {
		logrus.Warnf("Failed to add jump to %s from POSTROUTING: %v", masqChainName, err)
	}

	logrus.Infof("Outbound internet MASQUERADE chain %s installed for %s", masqChainName, subnet)
}

func teardownForwardChain() {
	removeChainReferences("filter", "FORWARD", forwardChain)

	exec.Command("sudo", "iptables", "-F", forwardChain).Run()
	exec.Command("sudo", "iptables", "-X", forwardChain).Run()
}

func teardownMasqueradeChain() {
	removeChainReferences("nat", "POSTROUTING", masqChainName)

	exec.Command("sudo", "iptables", "-t", "nat", "-F", masqChainName).Run()
	exec.Command("sudo", "iptables", "-t", "nat", "-X", masqChainName).Run()
}

func removeChainReferences(table, parentChain, childChain string) {
	args := []string{"iptables"}
	if table != "filter" {
		args = append(args, "-t", table)
	}
	args = append(args, "-S", parentChain)

	out, err := exec.Command("sudo", args...).Output()
	if err != nil {
		return
	}

	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, childChain) {
			continue
		}

		delRule := strings.Replace(line, "-A "+parentChain, "-D "+parentChain, 1)
		delArgs := []string{"iptables"}
		if table != "filter" {
			delArgs = append(delArgs, "-t", table)
		}
		delArgs = append(delArgs, strings.Fields(delRule)...)
		exec.Command("sudo", delArgs...).Run()
	}
}

func subnetForPrefix(prefix string) (string, error) {
	parts := strings.Split(prefix, ".")

	switch len(parts) {
	case 1:
		// A single octet means the forwarding and masquerade rules cover a whole
		// /8 of routable address space. Fine on an isolated worker, but the
		// operator should know the blast radius.
		logrus.Warnf("Firecracker IP prefix %q expands to a /8; consider a longer prefix to narrow the iptables rules", prefix)
		return prefix + ".0.0.0/8", nil
	case 2:
		return prefix + ".0.0/16", nil
	case 3:
		return prefix + ".0/24", nil
	default:
		return "", fmt.Errorf("unsupported prefix %q", prefix)
	}
}

// refreshNetworkAfterResume drops the neighbour entry a restored guest inherited
// from the snapshotted VM, then re-announces the gateway so the guest relearns
// it without waiting for its own ARP timeout.
func refreshNetworkAfterResume(cfg *NetworkConfig) {
	netns := cfg.NetNS
	tapDev := cfg.TapDeviceName
	guestIP := cfg.TapInternalIP
	gatewayIP := cfg.TapExternalIP

	if err := exec.Command("sudo", "ip", "netns", "exec", netns,
		"ip", "neigh", "flush", "dev", tapDev).Run(); err != nil {
		logrus.Warnf("snapshot-restore: failed to flush ARP on %s in %s: %v", tapDev, netns, err)
	}

	if !arpingAvailable {
		return
	}

	cmd := exec.Command("sudo", "ip", "netns", "exec", netns,
		"arping", "-c", "1", "-w", "1", "-f", "-I", tapDev, "-s", gatewayIP, guestIP)
	if err := cmd.Start(); err != nil {
		logrus.Warnf("snapshot-restore: failed to start arping to %s via %s in %s: %v", guestIP, tapDev, netns, err)
		return
	}

	go func() {
		if err := cmd.Wait(); err != nil {
			logrus.Debugf("snapshot-restore: arping to %s via %s in %s failed: %v", guestIP, tapDev, netns, err)
		}
	}()
}

func deleteDeviceByName(name string) {
	err := exec.Command("sudo", "ip", "link", "del", name).Run()
	if err != nil {
		logrus.Errorf("Failed to delete network device %s - %v.", name, err)
	}
}

func deleteNetworkNamespaceByName(name string) {
	err := exec.Command("sudo", "ip", "netns", "delete", name).Run()
	if err != nil {
		logrus.Errorf("Failed to delete namespace %s - %v.", name, err)
	}
}

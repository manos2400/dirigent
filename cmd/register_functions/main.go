package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"cluster_manager/pkg/utils"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

type function struct {
	Name              string         `yaml:"name"`
	Image             string         `yaml:"image"`
	PortForwarding    portForwarding `yaml:"port_forwarding"`
	RequestedCPU      *int           `yaml:"requested_cpu"`
	RequestedMemory   *int           `yaml:"requested_memory"`
	ScalingUpperBound *int           `yaml:"scaling_upper_bound"`
	ScalingLowerBound *int           `yaml:"scaling_lower_bound"`
	ColdStartSweep    *bool          `yaml:"cold_start_sweep"`
}

type portForwarding struct {
	GuestPort int    `yaml:"guest_port"`
	Protocol  string `yaml:"protocol"` // tcp|udp
}

var (
	filePath = flag.String("file", "cmd/register_functions/functions.yaml", "Path to the functions manifest YAML")
	address  = flag.String("address", "", "Override registration server address (host:port). Defaults to manifest.registration.address or 0.0.0.0:9091")
	timeout  = flag.Duration("timeout", 10*time.Second, "HTTP timeout per registration request")
)

func main() {
	flag.Parse()

	buf, err := os.ReadFile(*filePath)
	if err != nil {
		logrus.Fatalf("Failed to read manifest file %s: %v", *filePath, err)
	}

	regAddr := strings.TrimSpace(*address)
	if regAddr == "" {
		regAddr = fmt.Sprintf("%s:%s", utils.Localhost, utils.DefaultControlPlanePortServiceRegistration)
	}

	functions, err := decodeFunctions(buf)
	if err != nil {
		logrus.Fatalf("Failed to parse functions YAML: %v", err)
	}
	if len(functions) == 0 {
		logrus.Fatalf("No functions found in file (%s).", *filePath)
	}

	client := &http.Client{Timeout: *timeout}

	ok := 0
	failed := 0
	for _, fn := range functions {
		err := registerOne(client, regAddr, fn)
		if err != nil {
			failed++
			logrus.Errorf("Failed to register function %q: %v", fn.Name, err)
			continue
		}
		ok++
		logrus.Infof("Registered function %q", fn.Name)
	}

	if failed > 0 {
		logrus.Fatalf("Completed with errors (ok=%d, failed=%d)", ok, failed)
	}
	logrus.Infof("All functions registered (count=%d)", ok)
}

func decodeFunctions(buf []byte) ([]function, error) {
	dec := yaml.NewDecoder(bytes.NewReader(buf))

	functions := make([]function, 0)
	for {
		var fn function
		err := dec.Decode(&fn)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		// tolerate empty docs (e.g. trailing ---)
		if strings.TrimSpace(fn.Name) == "" && strings.TrimSpace(fn.Image) == "" {
			continue
		}

		if err := validateFunctionSpec(fn); err != nil {
			return nil, err
		}

		functions = append(functions, fn)
	}

	return functions, nil
}

func validateFunctionSpec(fn function) error {
	name := strings.TrimSpace(fn.Name)
	image := strings.TrimSpace(fn.Image)
	protocol := strings.ToLower(strings.TrimSpace(fn.PortForwarding.Protocol))

	if name == "" {
		return fmt.Errorf("invalid function spec: missing name")
	}
	if image == "" {
		return fmt.Errorf("invalid function %q spec: missing image", name)
	}
	if fn.PortForwarding.GuestPort <= 0 {
		return fmt.Errorf(
			"invalid function %q spec: port_forwarding.guest_port must be > 0 (got %d). If you wrote:\n  port_forwarding:\n  guest_port: 80\nthen indent guest_port/protocol under port_forwarding (two spaces).",
			name,
			fn.PortForwarding.GuestPort,
		)
	}
	if protocol != "tcp" && protocol != "udp" {
		return fmt.Errorf("invalid function %q spec: port_forwarding.protocol must be tcp|udp (got %q)", name, fn.PortForwarding.Protocol)
	}

	return nil
}

func registerOne(client *http.Client, address string, fn function) error {
	fn.Name = strings.TrimSpace(fn.Name)
	fn.Image = strings.TrimSpace(fn.Image)
	fn.PortForwarding.Protocol = strings.ToLower(strings.TrimSpace(fn.PortForwarding.Protocol))

	if fn.Name == "" {
		return fmt.Errorf("missing name")
	}
	if fn.Image == "" {
		return fmt.Errorf("missing image")
	}
	if fn.PortForwarding.GuestPort <= 0 {
		return fmt.Errorf("invalid port_forwarding.guest_port: %d", fn.PortForwarding.GuestPort)
	}
	if fn.PortForwarding.Protocol != "tcp" && fn.PortForwarding.Protocol != "udp" {
		return fmt.Errorf("invalid port_forwarding.protocol (expected tcp|udp): %q", fn.PortForwarding.Protocol)
	}

	form := url.Values{}
	form.Set("name", fn.Name)
	form.Set("image", fn.Image)
	form.Add("port_forwarding", strconv.Itoa(fn.PortForwarding.GuestPort))
	form.Add("port_forwarding", fn.PortForwarding.Protocol)

	if fn.RequestedCPU != nil {
		form.Set("requested_cpu", strconv.Itoa(*fn.RequestedCPU))
	}
	if fn.RequestedMemory != nil {
		form.Set("requested_memory", strconv.Itoa(*fn.RequestedMemory))
	}
	if fn.ScalingUpperBound != nil {
		form.Set("scaling_upper_bound", strconv.Itoa(*fn.ScalingUpperBound))
	}
	if fn.ScalingLowerBound != nil {
		form.Set("scaling_lower_bound", strconv.Itoa(*fn.ScalingLowerBound))
	}
	if fn.ColdStartSweep != nil {
		form.Set("cold_start_sweep", strconv.FormatBool(*fn.ColdStartSweep))
	}

	req, err := http.NewRequest(http.MethodPost, "http://"+address+"/", bytes.NewBufferString(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("registration server returned %s: %s", resp.Status, msg)
	}

	return nil
}

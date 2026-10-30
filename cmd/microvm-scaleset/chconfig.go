package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// chConfig holds the settings only the cloud-hypervisor backend uses.
type chConfig struct {
	guestManifest      string
	bridge             string
	subnet             string
	workDir            string
	vmUser             string
	memReservePercent  float64
	memReserveMinGiB   float64
	diskReservePercent float64
	nixDaemonPort      int
	controlPort        int
}

// guestManifest is what the host module tells the controller about the
// guest system: everything a cloud-hypervisor VM boots from.
type guestManifest struct {
	CloudHypervisor string       `json:"cloudHypervisor"`
	Virtiofsd       string       `json:"virtiofsd"`
	Kernel          string       `json:"kernel"`
	Initrd          string       `json:"initrd"`
	Cmdline         string       `json:"cmdline"`
	Shares          []guestShare `json:"shares"`
	BootMemMiB      int64        `json:"bootMemMiB"`
	HotplugMemMiB   int64        `json:"hotplugMemMiB"`
}

type guestShare struct {
	Tag    string `json:"tag"`
	Source string `json:"source"`
}

var shareTag = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,35}$`)

func parseGuestManifest(raw []byte) (guestManifest, error) {
	var m guestManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("guest manifest: %w", err)
	}
	for name, path := range map[string]string{"cloudHypervisor": m.CloudHypervisor, "virtiofsd": m.Virtiofsd, "kernel": m.Kernel, "initrd": m.Initrd} {
		if !filepath.IsAbs(path) {
			return m, fmt.Errorf("guest manifest: %s must be an absolute path", name)
		}
	}
	if strings.ContainsAny(m.Cmdline, "\r\n") {
		return m, errors.New("guest manifest: cmdline must be one line")
	}
	if m.BootMemMiB <= 0 || m.HotplugMemMiB <= 0 || m.BootMemMiB%128 != 0 || m.HotplugMemMiB%128 != 0 {
		return m, errors.New("guest manifest: bootMemMiB and hotplugMemMiB must be positive multiples of 128")
	}
	seen := map[string]bool{}
	for _, s := range m.Shares {
		if !shareTag.MatchString(s.Tag) || seen[s.Tag] || !filepath.IsAbs(s.Source) || strings.ContainsAny(s.Source, ",") {
			return m, fmt.Errorf("guest manifest: invalid share %q", s.Tag)
		}
		seen[s.Tag] = true
	}
	return m, nil
}

// vmMemory is a new VM's memory: the boot size, growing up to boot plus
// hotplug size.
func (m guestManifest) vmMemory() vmMemory {
	return vmMemory{bootBytes: m.BootMemMiB * mib, maxBytes: (m.BootMemMiB + m.HotplugMemMiB) * mib, actualBytes: m.BootMemMiB * mib}
}

// subnetPrefix returns the three octets of --subnet.
func subnetPrefix(subnet string) ([3]byte, error) {
	var b [3]byte
	parts := strings.Split(subnet, ".")
	if len(parts) != 3 {
		return b, errors.New("--subnet must be three octets, e.g. 10.231.0")
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 || strconv.Itoa(n) != p {
			return b, errors.New("--subnet must be three octets, e.g. 10.231.0")
		}
		b[i] = byte(n)
	}
	return b, nil
}

var runnerNamePrefix = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func (c config) validateCloudHypervisor() error {
	if c.ch.guestManifest == "" || c.ch.bridge == "" || c.ch.subnet == "" {
		return errors.New("--backend=cloud-hypervisor needs --guest-manifest, --bridge and --subnet")
	}
	if _, err := subnetPrefix(c.ch.subnet); err != nil {
		return err
	}
	if len(c.ch.bridge) > 15 || strings.ContainsAny(c.ch.bridge, "/ \t\r\n") {
		return errors.New("invalid --bridge")
	}
	if !filepath.IsAbs(c.ch.workDir) || c.ch.vmUser == "" {
		return errors.New("--work-dir must be absolute and --vm-user non-empty")
	}
	// The runner name is also the guest hostname and a directory.
	if !runnerNamePrefix.MatchString(c.name) || len(c.name) > 48 {
		return errors.New("--name must be at most 48 letters, digits, '.', '_' or '-'")
	}
	// Guest-to-host vsock connections arrive on <work-dir>/<runner>/vsock.sock_<port>,
	// and a Unix socket path holds at most 107 bytes.
	longest := filepath.Join(c.ch.workDir, c.name+"-254-00000000", fmt.Sprintf("vsock.sock_%d", max(c.ch.nixDaemonPort, c.ch.controlPort)))
	if len(longest) > 107 {
		return fmt.Errorf("--work-dir and --name too long for socket path %s", longest)
	}
	for _, v := range []float64{c.ch.memReservePercent, c.ch.diskReservePercent} {
		if math.IsNaN(v) || v < 0 || v >= 100 {
			return errors.New("reserve percentages must be in [0, 100)")
		}
	}
	if math.IsNaN(c.ch.memReserveMinGiB) || math.IsInf(c.ch.memReserveMinGiB, 0) || c.ch.memReserveMinGiB < 0 {
		return errors.New("--mem-reserve-min-gib must be finite and nonnegative")
	}
	if c.maxLoadPerCPU <= 0 || math.IsNaN(c.maxLoadPerCPU) || math.IsInf(c.maxLoadPerCPU, 0) {
		return errors.New("--max-load-per-cpu must be finite and positive")
	}
	for _, port := range []int{c.ch.nixDaemonPort, c.ch.controlPort} {
		if port <= 0 || port > math.MaxUint32 {
			return errors.New("invalid vsock port")
		}
	}
	if c.ch.nixDaemonPort == c.ch.controlPort {
		return errors.New("--nix-daemon-port and --control-port must differ")
	}
	return nil
}

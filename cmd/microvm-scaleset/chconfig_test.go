package main

import (
	"slices"
	"strings"
	"testing"
)

func TestCloudHypervisorFlags(t *testing.T) {
	if c, err := parseConfig(nil); err != nil || c.backend != backendQEMU {
		t.Fatalf("default backend = %q, %v; want qemu", c.backend, err)
	}
	base := []string{"--backend=cloud-hypervisor", "--guest-manifest=/m.json", "--bridge=microbr", "--subnet=10.231.0", "--name=ci"}
	c, err := parseConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	if c.backend != backendCloudHypervisor || c.ch.workDir != "/var/lib/microvm-scaleset" || c.ch.vmUser != "microvm" || c.ch.memReservePercent != 10 || c.ch.memReserveMinGiB != 2 || c.ch.diskReservePercent != 10 || c.ch.nixDaemonPort != 5000 || c.ch.controlPort != 5001 {
		t.Fatalf("defaults = %+v", c)
	}
	for _, bad := range [][]string{
		{"--backend=cloud-hypervisor", "--name=ci"},
		{"--backend=cloud-hypervisor", "--guest-manifest=/m.json", "--bridge=microbr", "--subnet=10.231", "--name=ci"},
		{"--backend=cloud-hypervisor", "--guest-manifest=/m.json", "--bridge=microbr", "--subnet=10.231.300", "--name=ci"},
		append(slices.Clone(base), "--mem-reserve-percent=100"),
		append(slices.Clone(base), "--control-port=5000"),
		append(slices.Clone(base), "--work-dir=relative"),
		append(slices.Clone(base), "--work-dir=/"+strings.Repeat("x", 80)),
		append(slices.Clone(base), "--backend=xen"),
	} {
		if _, err := parseConfig(bad); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	// QEMU-only settings are not needed with cloud-hypervisor, nor CH ones with QEMU.
	if _, err := parseConfig([]string{"--backend=qemu", "--slots=a,b"}); err != nil {
		t.Fatal(err)
	}
}

func TestGuestManifestValidation(t *testing.T) {
	good := `{"cloudHypervisor":"/ch","virtiofsd":"/v","kernel":"/k","initrd":"/i","cmdline":"x","shares":[{"tag":"ro-store","source":"/nix/store"}],"bootMemMiB":2048,"hotplugMemMiB":1048576}`
	m, err := parseGuestManifest([]byte(good))
	if err != nil || m.BootMemMiB != 2048 || m.Shares[0].Tag != "ro-store" {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	for _, bad := range []string{
		strings.Replace(good, `"/ch"`, `"ch"`, 1),
		strings.Replace(good, `2048`, `0`, 1),
		strings.Replace(good, `2048`, `2000`, 1),
		strings.Replace(good, `"ro-store"`, `"a,b"`, 1),
		strings.Replace(good, `"/nix/store"`, `"/nix/store,x"`, 1),
		`{`,
	} {
		if _, err := parseGuestManifest([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

// A cloud-hypervisor host ignores the QEMU pool's settings, even invalid
// ones an old module still passes.
func TestQEMUSettingsIgnoredWithCloudHypervisor(t *testing.T) {
	args := []string{"--backend=cloud-hypervisor", "--guest-manifest=/m.json", "--bridge=microbr", "--subnet=10.231.0", "--name=ci", "--vm-mem=0", "--mem-reserve=-1", "--slots=../x"}
	if _, err := parseConfig(args); err != nil {
		t.Fatal(err)
	}
}

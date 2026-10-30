package main

import (
	"bufio"
	"net"
	"path/filepath"
	"slices"
	"testing"
)

func TestVMSpecDryRun(t *testing.T) {
	c := config{name: "ci", ch: chConfig{workDir: "/var/lib/microvm-scaleset", vmUser: "microvm", nixDaemonPort: 5000, controlPort: 5001}}
	addr := vmAddress{[3]byte{10, 231, 0}, 12}
	runner := "ci-12-0123abcd"
	spec := buildVMSpec(c, testManifest(), runner, addr, 16)
	unit := vmUnit(c.name, addr)
	systemdRun := systemdRunArgv(unit, "/nix/store/scaleset/bin/microvm-scaleset", "/var/lib/microvm-scaleset/"+runner+".json")
	t.Logf("systemd-run argv: %q", systemdRun)
	for _, fs := range spec.Virtiofsd {
		t.Logf("virtiofsd argv: %q", fs.Argv)
	}
	t.Logf("nix-proxy: listen %s as %s, forward to %s", spec.NixSocket, spec.NixUser, spec.NixTarget)
	t.Logf("cloud-hypervisor argv (as %s): %q", spec.User, spec.CloudHypervisor)

	dir := "/var/lib/microvm-scaleset/ci-12-0123abcd"
	want := []string{"/nix/store/ch/bin/cloud-hypervisor",
		"--api-socket", "path=" + dir + "/api.sock",
		"--kernel", "/nix/store/kernel/bzImage",
		"--initramfs", "/nix/store/initrd/initrd",
		"--cmdline", "console=ttyS0 init=/nix/store/system/init ghr.name=ci-12-0123abcd ghr.addr=10.231.0.12/24 ghr.gw=10.231.0.1",
		"--cpus", "boot=16,max=16",
		"--memory", "size=2048M,hotplug_method=virtio-mem,hotplug_size=1048576M,shared=on",
		"--balloon", "free_page_reporting=on",
		"--disk", "path=" + dir + "/disk.img,direct=on",
		"--net", "tap=ghrunner12,mac=02:00:0a:e7:00:0c",
		"--vsock", "cid=5,socket=" + dir + "/vsock.sock",
		"--serial", "tty",
		"--console", "off",
		"--fs", "tag=ro-store,socket=" + dir + "/fs-ro-store.sock",
	}
	if !slices.Equal(spec.CloudHypervisor, want) {
		t.Fatalf("cloud-hypervisor argv\n got %q\nwant %q", spec.CloudHypervisor, want)
	}
	if spec.NixSocket != dir+"/vsock.sock_5000" || spec.NixUser != "microvm-nix" || spec.NixTarget != "/nix/var/nix/daemon-socket/socket" {
		t.Fatalf("nix forwarder = %+v", spec)
	}
	if !slices.Equal(spec.Virtiofsd[0].Argv, []string{"/nix/store/vfs/bin/virtiofsd", "--socket-path=" + dir + "/fs-ro-store.sock", "--shared-dir=/nix/store", "--readonly", "--cache=auto"}) {
		t.Fatalf("virtiofsd argv = %q", spec.Virtiofsd[0].Argv)
	}
	if unit != "ghr-ci-12.service" || !slices.Contains(systemdRun, "--property=CPUWeight=100") || !slices.Contains(systemdRun, "--property=CPUAccounting=yes") || !slices.Contains(systemdRun, "--unit=ghr-ci-12.service") || systemdRun[len(systemdRun)-2] != "vm" {
		t.Fatalf("systemd-run argv = %q", systemdRun)
	}
}

func TestNixProxyForwardsBothWays(t *testing.T) {
	dir := t.TempDir()
	daemon, err := net.Listen("unix", filepath.Join(dir, "daemon"))
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	go func() {
		conn, err := daemon.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		conn.Write([]byte("daemon got " + line))
	}()
	front, err := net.Listen("unix", filepath.Join(dir, "vsock.sock_5000"))
	if err != nil {
		t.Fatal(err)
	}
	defer front.Close()
	go proxyUnix(front, filepath.Join(dir, "daemon"))
	conn, err := net.Dial("unix", filepath.Join(dir, "vsock.sock_5000"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("hello\n"))
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || reply != "daemon got hello\n" {
		t.Fatalf("reply %q, %v", reply, err)
	}
}

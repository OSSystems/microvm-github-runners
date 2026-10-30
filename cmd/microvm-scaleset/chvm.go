package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const nixDaemonSocket = "/nix/var/nix/daemon-socket/socket"

// vmSpec is everything the per-VM supervisor runs, written by the controller
// before it starts the VM's unit.
type vmSpec struct {
	Runner          string       `json:"runner"`
	Dir             string       `json:"dir"`
	User            string       `json:"user"`
	NixUser         string       `json:"nixUser"`
	Virtiofsd       []vmFsDaemon `json:"virtiofsd"`
	NixSocket       string       `json:"nixSocket"`
	NixTarget       string       `json:"nixTarget"`
	CloudHypervisor []string     `json:"cloudHypervisor"`
}

type vmFsDaemon struct {
	Socket string   `json:"socket"`
	Argv   []string `json:"argv"`
}

func vsockSocket(dir string) string { return filepath.Join(dir, "vsock.sock") }

func vsockPortSocket(dir string, port int) string {
	return fmt.Sprintf("%s_%d", vsockSocket(dir), port)
}

func apiSocket(dir string) string { return filepath.Join(dir, "api.sock") }

func diskImage(dir string) string { return filepath.Join(dir, "disk.img") }

// buildVMSpec derives one VM's processes from the guest manifest and its
// address. The VM gets every host CPU; memory starts at the boot size and
// grows through virtio-mem.
func buildVMSpec(c config, m guestManifest, runner string, addr vmAddress, cpus int) vmSpec {
	dir := filepath.Join(c.ch.workDir, runner)
	spec := vmSpec{
		Runner:    runner,
		Dir:       dir,
		User:      c.ch.vmUser,
		NixUser:   "microvm-nix",
		NixSocket: vsockPortSocket(dir, c.ch.nixDaemonPort),
		NixTarget: nixDaemonSocket,
	}
	cmdline := strings.TrimSpace(m.Cmdline + " ghr.name=" + runner + " ghr.addr=" + addr.ip() + "/24 ghr.gw=" + addr.gateway())
	ch := []string{m.CloudHypervisor,
		"--api-socket", "path=" + apiSocket(dir),
		"--kernel", m.Kernel,
		"--initramfs", m.Initrd,
		"--cmdline", cmdline,
		"--cpus", fmt.Sprintf("boot=%d,max=%d", cpus, cpus),
		"--memory", fmt.Sprintf("size=%dM,hotplug_method=virtio-mem,hotplug_size=%dM,shared=on", m.BootMemMiB, m.HotplugMemMiB),
		"--balloon", "free_page_reporting=on",
		"--disk", "path=" + diskImage(dir) + ",direct=on",
		"--net", "tap=" + addr.tap() + ",mac=" + addr.mac(),
		"--vsock", fmt.Sprintf("cid=%d,socket=%s", addr.cid(), vsockSocket(dir)),
		"--serial", "tty",
		"--console", "off",
	}
	for _, share := range m.Shares {
		socket := filepath.Join(dir, "fs-"+share.Tag+".sock")
		spec.Virtiofsd = append(spec.Virtiofsd, vmFsDaemon{Socket: socket, Argv: []string{m.Virtiofsd,
			"--socket-path=" + socket,
			"--shared-dir=" + share.Source,
			"--readonly",
			"--cache=auto",
		}})
		ch = append(ch, "--fs", "tag="+share.Tag+",socket="+socket)
	}
	spec.CloudHypervisor = ch
	return spec
}

// vmUnit is the transient unit of the VM at an address; stable per address so
// a leftover is found and stopped before the address is reused.
func vmUnit(name string, addr vmAddress) string {
	return fmt.Sprintf("ghr-%s-%d.service", name, addr.octet)
}

// systemdRunArgv starts the supervisor of one VM in its own transient unit,
// with CPU accounting and an equal CPU share.
func systemdRunArgv(unit, self, specPath string) []string {
	return []string{"systemd-run",
		"--unit=" + unit,
		"--collect",
		"--quiet",
		"--description=GitHub runner VM " + strings.TrimSuffix(unit, ".service"),
		"--property=CPUWeight=100",
		"--property=CPUAccounting=yes",
		"--property=MemoryAccounting=yes",
		"--property=TasksAccounting=yes",
		"--property=KillMode=control-group",
		"--property=TimeoutStopSec=30s",
		"--",
		self, "vm", "--spec=" + specPath,
	}
}

func (a account) credential() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(a.uid), Gid: uint32(a.gid), Groups: a.groups}, Pdeathsig: syscall.SIGTERM}
}

// runVM is the main process of a VM's unit: virtiofsd per share (root), the
// Nix daemon forwarder (as the Nix user) and cloud-hypervisor (as the VM
// user). It ends with cloud-hypervisor and takes the helpers with it; when a
// helper dies first, the VM is stopped too.
func runVM(args []string) error {
	f := flag.NewFlagSet("vm", flag.ContinueOnError)
	specPath := f.String("spec", "", "VM spec written by the controller")
	if err := f.Parse(args); err != nil {
		return err
	}
	raw, err := readBounded(*specPath, 1<<20)
	if err != nil {
		return err
	}
	var spec vmSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return err
	}
	vmAccount, err := lookupAccount(spec.User)
	if err != nil {
		return fmt.Errorf("VM user: %w", err)
	}
	nixAccount, err := lookupAccount(spec.NixUser)
	if err != nil {
		return fmt.Errorf("Nix user: %w", err)
	}

	var helpers []*exec.Cmd
	exited := make(chan string, len(spec.Virtiofsd)+2)
	stopHelpers := func() {
		for _, h := range helpers {
			h.Process.Signal(syscall.SIGTERM)
		}
	}
	defer stopHelpers()
	start := func(cmd *exec.Cmd, what string) error {
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("starting %s: %w", what, err)
		}
		helpers = append(helpers, cmd)
		go func() { cmd.Wait(); exited <- what }()
		return nil
	}

	for _, fs := range spec.Virtiofsd {
		cmd := exec.Command(fs.Argv[0], fs.Argv[1:]...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
		if err := start(cmd, "virtiofsd "+fs.Socket); err != nil {
			return err
		}
		if err := waitForSocket(fs.Socket, 10*time.Second); err != nil {
			return err
		}
		if err := os.Chown(fs.Socket, vmAccount.uid, vmAccount.gid); err != nil {
			return err
		}
	}

	os.Remove(spec.NixSocket)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: spec.NixSocket, Net: "unix"})
	if err != nil {
		return err
	}
	ln.SetUnlinkOnClose(false)
	if err := os.Chown(spec.NixSocket, vmAccount.uid, vmAccount.gid); err != nil {
		return err
	}
	if err := os.Chmod(spec.NixSocket, 0600); err != nil {
		return err
	}
	lnFile, err := ln.File()
	ln.Close()
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	proxy := exec.Command(self, "nix-proxy", "--target="+spec.NixTarget)
	proxy.ExtraFiles = []*os.File{lnFile}
	proxy.SysProcAttr = nixAccount.credential()
	err = start(proxy, "nix-proxy")
	lnFile.Close()
	if err != nil {
		return err
	}

	ch := exec.Command(spec.CloudHypervisor[0], spec.CloudHypervisor[1:]...)
	ch.SysProcAttr = vmAccount.credential()
	ch.Stdout, ch.Stderr = os.Stdout, os.Stderr
	ch.Dir = spec.Dir
	if err := ch.Start(); err != nil {
		return fmt.Errorf("starting cloud-hypervisor: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- ch.Wait() }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	for {
		select {
		case err := <-done:
			return err
		case what := <-exited:
			fmt.Fprintf(os.Stderr, "%s exited; stopping the VM\n", what)
			ch.Process.Signal(syscall.SIGTERM)
		case <-signals:
			ch.Process.Signal(syscall.SIGTERM)
		}
	}
}

func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not appear", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// runNixProxy forwards every connection on the inherited listener (fd 3) to
// the host Nix daemon, as the unprivileged user it runs as, so guests are
// untrusted daemon clients.
func runNixProxy(args []string) error {
	f := flag.NewFlagSet("nix-proxy", flag.ContinueOnError)
	target := f.String("target", nixDaemonSocket, "Nix daemon socket")
	if err := f.Parse(args); err != nil {
		return err
	}
	ln, err := net.FileListener(os.NewFile(3, "listener"))
	if err != nil {
		return err
	}
	return proxyUnix(ln, *target)
}

func proxyUnix(ln net.Listener, target string) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer conn.Close()
			upstream, err := net.Dial("unix", target)
			if err != nil {
				return
			}
			defer upstream.Close()
			var wg sync.WaitGroup
			wg.Go(func() { io.Copy(upstream, conn); closeWrite(upstream) })
			io.Copy(conn, upstream)
			closeWrite(conn)
			wg.Wait()
		}()
	}
}

func closeWrite(c net.Conn) {
	if u, ok := c.(*net.UnixConn); ok {
		u.CloseWrite()
	}
}

var errNotSubcommand = errors.New("not a subcommand")

// runSubcommand runs the per-VM helper modes of this binary.
func runSubcommand(args []string) error {
	if len(args) == 0 {
		return errNotSubcommand
	}
	switch args[0] {
	case "vm":
		return runVM(args[1:])
	case "nix-proxy":
		return runNixProxy(args[1:])
	}
	return errNotSubcommand
}

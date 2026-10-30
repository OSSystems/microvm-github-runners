package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

const (
	firstOctet = 10
	lastOctet  = 254
)

// vmAddress is everything derived from a VM's place in the /24: its IP, vsock
// CID, MAC and tap.
type vmAddress struct {
	subnet [3]byte
	octet  int
}

func (a vmAddress) ip() string {
	return fmt.Sprintf("%d.%d.%d.%d", a.subnet[0], a.subnet[1], a.subnet[2], a.octet)
}

func (a vmAddress) gateway() string {
	return fmt.Sprintf("%d.%d.%d.1", a.subnet[0], a.subnet[1], a.subnet[2])
}

func (a vmAddress) cid() int { return 3 + a.octet - firstOctet }

// mac is 02:00 followed by the four octets of the VM's IP. The contract's
// 02:00:0a:e7:XX:YY is this for the default subnet 10.231.0; other subnets
// change the middle bytes too, which nothing depends on: the guest matches
// its NIC by driver, not by MAC.
func (a vmAddress) mac() string {
	return fmt.Sprintf("02:00:%02x:%02x:%02x:%02x", a.subnet[0], a.subnet[1], a.subnet[2], a.octet)
}

func (a vmAddress) tap() string { return fmt.Sprintf("ghrunner%d", a.octet) }

// addressPool hands out the VM addresses .10 to .254 of the subnet, lowest
// first.
type addressPool struct {
	subnet [3]byte
	used   map[int]bool
}

func newAddressPool(subnet [3]byte) *addressPool {
	return &addressPool{subnet: subnet, used: map[int]bool{}}
}

func (p *addressPool) size() int { return lastOctet - firstOctet + 1 }

func (p *addressPool) allocate() (vmAddress, bool) {
	for o := firstOctet; o <= lastOctet; o++ {
		if !p.used[o] {
			p.used[o] = true
			return vmAddress{p.subnet, o}, true
		}
	}
	return vmAddress{}, false
}

func (p *addressPool) release(a vmAddress) { delete(p.used, a.octet) }

// Linux interface ioctls, from <linux/if_tun.h> and <linux/sockios.h>.
const (
	tunSetIff     = 0x400454ca
	tunSetPersist = 0x400454cb
	tunSetOwner   = 0x400454cc
	iffTap        = 0x0002
	iffNoPI       = 0x1000
	iffVnetHdr    = 0x4000
	siocBrAddIf   = 0x89a2
)

type ifreq struct {
	name [16]byte
	data [24]byte
}

func newIfreq(name string) (*ifreq, error) {
	if len(name) == 0 || len(name) > 15 {
		return nil, fmt.Errorf("invalid interface name %q", name)
	}
	r := &ifreq{}
	copy(r.name[:], name)
	return r, nil
}

func ioctl(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

func ioctlValue(fd uintptr, req uintptr, value uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, value); errno != 0 {
		return errno
	}
	return nil
}

// netOps creates and deletes VM taps; tests replace it.
type netOps interface {
	createTap(name string, uid int, bridge string) error
	deleteTap(name string) error
}

type linuxNet struct{}

// createTap creates a persistent tap owned by uid, enslaves it to bridge and
// brings it up, so an unprivileged cloud-hypervisor can attach to it without
// CAP_NET_ADMIN.
func (linuxNet) createTap(name string, uid int, bridge string) error {
	tun, err := openTap(name)
	if err != nil {
		return err
	}
	defer tun.Close()
	if err := ioctlValue(tun.Fd(), tunSetOwner, uintptr(uid)); err != nil {
		return fmt.Errorf("setting owner of tap %s: %w", name, err)
	}
	if err := ioctlValue(tun.Fd(), tunSetPersist, 1); err != nil {
		return fmt.Errorf("making tap %s persistent: %w", name, err)
	}
	sock, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(sock)
	index := &ifreq{}
	copy(index.name[:], name)
	if err := ioctl(uintptr(sock), syscall.SIOCGIFINDEX, unsafe.Pointer(index)); err != nil {
		return fmt.Errorf("looking up tap %s: %w", name, err)
	}
	br, err := newIfreq(bridge)
	if err != nil {
		return err
	}
	copy(br.data[:4], index.data[:4])
	if err := ioctl(uintptr(sock), siocBrAddIf, unsafe.Pointer(br)); err != nil {
		return fmt.Errorf("adding tap %s to bridge %s: %w", name, bridge, err)
	}
	up := &ifreq{}
	copy(up.name[:], name)
	if err := ioctl(uintptr(sock), syscall.SIOCGIFFLAGS, unsafe.Pointer(up)); err != nil {
		return err
	}
	*(*uint16)(unsafe.Pointer(&up.data[0])) |= syscall.IFF_UP
	if err := ioctl(uintptr(sock), syscall.SIOCSIFFLAGS, unsafe.Pointer(up)); err != nil {
		return fmt.Errorf("bringing tap %s up: %w", name, err)
	}
	return nil
}

// deleteTap removes a persistent tap; a missing one is not an error.
func (linuxNet) deleteTap(name string) error {
	if _, err := os.Stat(filepath.Join("/sys/class/net", name)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	tun, err := openTap(name)
	if err != nil {
		return err
	}
	defer tun.Close()
	return ioctlValue(tun.Fd(), tunSetPersist, 0)
}

// openTap attaches to the tap called name, creating it if missing, with the
// flags cloud-hypervisor opens it with.
func openTap(name string) (*os.File, error) {
	r, err := newIfreq(name)
	if err != nil {
		return nil, err
	}
	tun, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	*(*uint16)(unsafe.Pointer(&r.data[0])) = iffTap | iffNoPI | iffVnetHdr
	if err := ioctl(tun.Fd(), tunSetIff, unsafe.Pointer(r)); err != nil {
		tun.Close()
		return nil, fmt.Errorf("opening tap %s: %w", name, err)
	}
	return tun, nil
}

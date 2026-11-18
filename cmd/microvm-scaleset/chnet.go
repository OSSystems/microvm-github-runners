package main

import (
	"fmt"

	"github.com/OSSystems/vmkit-nix/tap"
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

// netOps creates and deletes VM taps; tests replace it.
type netOps interface {
	createTap(name string, uid int, bridge string) error
	deleteTap(name string) error
}

type linuxNet struct{}

func (linuxNet) createTap(name string, uid int, bridge string) error {
	return tap.Create(name, uid, bridge)
}

func (linuxNet) deleteTap(name string) error { return tap.Delete(name) }

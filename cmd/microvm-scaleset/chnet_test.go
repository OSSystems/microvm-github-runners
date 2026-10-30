package main

import "testing"

func TestAddressPool(t *testing.T) {
	p := newAddressPool([3]byte{10, 231, 0})
	first, ok := p.allocate()
	if !ok || first.ip() != "10.231.0.10" || first.cid() != 3 || first.mac() != "02:00:0a:e7:00:0a" || first.tap() != "ghrunner10" || first.gateway() != "10.231.0.1" {
		t.Fatalf("first address = %s cid %d mac %s tap %s", first.ip(), first.cid(), first.mac(), first.tap())
	}
	n := 1
	var last vmAddress
	for {
		a, ok := p.allocate()
		if !ok {
			break
		}
		last = a
		n++
	}
	if n != 245 || p.size() != 245 || last.ip() != "10.231.0.254" || last.cid() != 247 || last.mac() != "02:00:0a:e7:00:fe" {
		t.Fatalf("allocated %d, last %s cid %d mac %s", n, last.ip(), last.cid(), last.mac())
	}
	p.release(vmAddress{p.subnet, 42})
	p.release(vmAddress{p.subnet, 17})
	if a, _ := p.allocate(); a.octet != 17 {
		t.Fatalf("reused .%d, want the lowest free .17", a.octet)
	}
	if a, _ := p.allocate(); a.octet != 42 {
		t.Fatalf("reused .%d, want .42", a.octet)
	}
	if _, ok := p.allocate(); ok {
		t.Fatal("allocated beyond .254")
	}
}

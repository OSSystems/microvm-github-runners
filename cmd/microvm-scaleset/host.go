package main

import (
	"github.com/OSSystems/vmkit-nix/hostutil"
	"github.com/OSSystems/vmkit-nix/memory"
)

const gib = memory.GiB

type account = hostutil.Account

var (
	lookupAccount = hostutil.LookupAccount
	unitRunning   = hostutil.UnitRunning
	load1         = hostutil.Load1
	meminfoKiB    = hostutil.MeminfoKiB
	readBounded   = hostutil.ReadBounded
	atomicWrite   = hostutil.AtomicWrite
	waitForSocket = hostutil.WaitForSocket
)

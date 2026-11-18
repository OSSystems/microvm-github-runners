package main

import (
	"fmt"

	"github.com/OSSystems/vmkit-nix/memory"
)

const (
	mib          = memory.MiB
	memoryStep   = memory.MemoryStep
	hostFullHold = memory.HostFullHold
)

type (
	guestStats   = memory.GuestStats
	hostMemory   = memory.HostMemory
	memoryAction = memory.MemoryAction
	vmMemory     = memory.VMMemory
)

const (
	memoryHold   = memory.MemoryHold
	memoryPlug   = memory.MemoryPlug
	memoryUnplug = memory.MemoryUnplug
	memoryDenied = memory.MemoryDenied
)

var memoryDecision = memory.Decide

// memoryReserve is what the host keeps for itself: the larger of a share of
// its memory and a floor.
func memoryReserve(c config, totalBytes int64) int64 {
	return memory.Reserve(totalBytes, c.ch.memReservePercent, c.ch.memReserveMinGiB)
}

// hostState is what admission looks at.
type hostState struct {
	load1        float64
	cpus         int
	memAvailable int64
	memTotal     int64
	diskFree     int64
	diskTotal    int64
}

// admit reports why a new VM may not boot, or "" when it may: CPU load,
// running guests recently denied memory (hostFull), memory for its boot size
// on top of what is pending, room left by what running VMs were given
// (committed) for it and its next plug, and disk space.
func admit(c config, h hostState, hostFull bool, bootBytes, pending, committed int64) string {
	if h.cpus > 0 && h.load1/float64(h.cpus) >= c.maxLoadPerCPU {
		return fmt.Sprintf("load %.2f per CPU", h.load1/float64(h.cpus))
	}
	if hostFull {
		return "running guests were denied memory"
	}
	reserve := memoryReserve(c, h.memTotal)
	if h.memAvailable-reserve < bootBytes+pending {
		return fmt.Sprintf("%.1f GiB available, %.1f GiB reserved, %.1f GiB pending", float64(h.memAvailable)/float64(gib), float64(reserve)/float64(gib), float64(pending)/float64(gib))
	}
	if h.memTotal-reserve-committed < bootBytes+memoryStep {
		return fmt.Sprintf("%.1f GiB committed to running VMs, %.1f GiB reserved", float64(committed)/float64(gib), float64(reserve)/float64(gib))
	}
	diskReserve := int64(float64(h.diskTotal) * c.ch.diskReservePercent / 100)
	if h.diskFree <= diskReserve {
		return fmt.Sprintf("%.1f GiB free on the work dir", float64(h.diskFree)/float64(gib))
	}
	return ""
}

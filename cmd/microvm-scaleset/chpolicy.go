package main

import (
	"fmt"
	"math"
	"time"
)

const (
	mib = int64(1) << 20
	// memoryStep is how much memory one plug or unplug moves.
	memoryStep = gib
	// statsStale is how old guest stats may be before memory stops changing.
	statsStale = 10 * time.Second
	// unplugCalm is how long a guest must have had spare memory and no
	// pressure before memory is taken back.
	unplugCalm = 30 * time.Second
	// plugSettle is how long a plug may take to show in the guest's MemTotal
	// before it stops counting as pending.
	plugSettle = time.Minute
	// plugSlack absorbs memory the guest kernel keeps for itself out of a plug.
	plugSlack = 128 * mib
	// hostFullHold keeps admission closed for this long after a plug was
	// denied: running guests come first.
	hostFullHold = 30 * time.Second
)

// guestStats is one report a guest sent on the control channel.
type guestStats struct {
	MemAvailableKiB int64     `json:"memAvailableKiB"`
	MemTotalKiB     int64     `json:"memTotalKiB"`
	PSISomeAvg10    float64   `json:"psiSomeAvg10"`
	PSIFullAvg10    float64   `json:"psiFullAvg10"`
	ReceivedAt      time.Time `json:"receivedAt"`
}

func (s guestStats) valid() bool {
	return s.MemAvailableKiB >= 0 && s.MemTotalKiB > 0 && s.MemAvailableKiB <= s.MemTotalKiB &&
		finiteNonNegative(s.PSISomeAvg10) && finiteNonNegative(s.PSIFullAvg10) && s.PSISomeAvg10 <= 100 && s.PSIFullAvg10 <= 100
}

func finiteNonNegative(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

func (s guestStats) available() int64 { return s.MemAvailableKiB << 10 }

// pressured is when a guest needs more memory.
func (s guestStats) pressured() bool { return s.PSISomeAvg10 > 5 || s.available() < gib }

// calm is when a guest could give memory back.
func (s guestStats) calm() bool { return s.PSISomeAvg10 < 0.1 && s.available() > 3*gib }

// hostMemory is the host side of a memory decision, in bytes.
type hostMemory struct {
	available int64
	reserve   int64
	// pending is memory promised but not yet taken: VMs that have not
	// reported yet and plugs their guest has not reported since.
	pending int64
}

// spare is what may still be handed out without touching the reserve.
func (h hostMemory) spare() int64 { return h.available - h.reserve - h.pending }

type memoryAction int

const (
	memoryHold memoryAction = iota
	memoryPlug
	memoryUnplug
	memoryDenied
)

func (a memoryAction) String() string {
	return [...]string{"hold", "plug", "unplug", "denied"}[a]
}

// vmMemory is what the policy knows about one VM.
type vmMemory struct {
	bootBytes int64
	maxBytes  int64
	// actualBytes is boot memory plus what virtio-mem has plugged; an unplug
	// may leave it above the requested size.
	actualBytes int64
	stats       guestStats
	// resizedAt is the last resize request; stats older than it do not show
	// its effect yet.
	resizedAt time.Time
	// calmSince is when the guest last became calm; zero when it is not.
	calmSince time.Time
	// pendingPlug is the last plug while the guest's MemTotal does not show
	// it yet; plugBaseKiB is the MemTotal it was granted on.
	pendingPlug int64
	plugBaseKiB int64
}

// observe takes a guest report into account: calm tracking, and the end of
// a pending plug once MemTotal shows it.
func (m *vmMemory) observe(s guestStats) {
	if m.stats.ReceivedAt.After(s.ReceivedAt) {
		return
	}
	m.stats = s
	if s.calm() {
		if m.calmSince.IsZero() {
			m.calmSince = s.ReceivedAt
		}
	} else {
		m.calmSince = time.Time{}
	}
	if m.pendingPlug > 0 && s.ReceivedAt.After(m.resizedAt) &&
		(s.MemTotalKiB<<10 >= m.plugBaseKiB<<10+m.pendingPlug-plugSlack || s.ReceivedAt.Sub(m.resizedAt) > plugSettle) {
		m.pendingPlug = 0
	}
}

// grant records a resize from actual to target before it is requested, so
// a plug counts as pending from the moment it is decided. It returns the
// state to restore if the request fails.
func (m *vmMemory) grant(actual, target int64, at time.Time) vmMemory {
	previous := *m
	m.resizedAt = at
	m.pendingPlug = 0
	if target > actual {
		m.pendingPlug = target - actual
		m.plugBaseKiB = m.stats.MemTotalKiB
	} else {
		// One unplug per calm period: the guest must stay calm for another
		// unplugCalm before the next step.
		m.calmSince = time.Time{}
	}
	return previous
}

// memoryDecision decides one VM's next memory change: plug 1 GiB under guest
// pressure if the host keeps its reserve, unplug 1 GiB after 30 s of calm,
// never below boot memory, and nothing on stale stats, before the guest
// reported after the last resize, or while the last plug is not visible yet.
func memoryDecision(vm vmMemory, host hostMemory, now time.Time) (memoryAction, int64) {
	s := vm.stats
	if s.ReceivedAt.IsZero() || now.Sub(s.ReceivedAt) > statsStale || s.ReceivedAt.After(now.Add(time.Second)) || !s.valid() {
		return memoryHold, vm.actualBytes
	}
	if !vm.resizedAt.IsZero() && !s.ReceivedAt.After(vm.resizedAt) || vm.pendingPlug > 0 {
		return memoryHold, vm.actualBytes
	}
	if s.pressured() {
		target := min(vm.maxBytes, vm.actualBytes+memoryStep)
		if target <= vm.actualBytes {
			return memoryHold, vm.actualBytes
		}
		if host.spare() < target-vm.actualBytes {
			return memoryDenied, vm.actualBytes
		}
		return memoryPlug, target
	}
	if s.calm() && !vm.calmSince.IsZero() && now.Sub(vm.calmSince) >= unplugCalm {
		target := max(vm.bootBytes, vm.actualBytes-memoryStep)
		if target < vm.actualBytes {
			return memoryUnplug, target
		}
	}
	return memoryHold, vm.actualBytes
}

// memoryReserve is what the host keeps for itself: the larger of a share of
// its memory and a floor.
func memoryReserve(c config, totalBytes int64) int64 {
	return max(int64(float64(totalBytes)*c.ch.memReservePercent/100), int64(c.ch.memReserveMinGiB*float64(gib)))
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
// on top of what is pending, and disk space.
func admit(c config, h hostState, hostFull bool, bootBytes, pending int64) string {
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
	diskReserve := int64(float64(h.diskTotal) * c.ch.diskReservePercent / 100)
	if h.diskFree <= diskReserve {
		return fmt.Sprintf("%.1f GiB free on the work dir", float64(h.diskFree)/float64(gib))
	}
	return ""
}

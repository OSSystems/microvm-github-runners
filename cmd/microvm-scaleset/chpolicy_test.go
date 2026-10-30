package main

import (
	"strings"
	"testing"
	"time"
)

func chTestConfig() config {
	return config{maxLoadPerCPU: 1, ch: chConfig{memReservePercent: 10, memReserveMinGiB: 2, diskReservePercent: 10}}
}

func TestAdmission(t *testing.T) {
	boot := 2 * gib
	ok := hostState{load1: 4, cpus: 16, memTotal: 64 * gib, memAvailable: 20 * gib, diskFree: 500 * gib, diskTotal: 1000 * gib}
	tests := []struct {
		name    string
		change  func(*hostState)
		pending int64
		want    string
	}{
		{"room", func(*hostState) {}, 0, ""},
		{"load at the limit", func(h *hostState) { h.load1 = 16 }, 0, "load"},
		// Reserve is max(10% of 64 GiB, 2 GiB) = 6.4 GiB; 6.4 + 2 boot + 12 pending > 20.
		{"pending boots count", func(*hostState) {}, 12 * gib, "pending"},
		{"pending boots that still fit", func(*hostState) {}, 11 * gib / 2, ""},
		{"percent reserve", func(h *hostState) { h.memAvailable = 8 * gib }, 0, "reserved"},
		{"exactly enough", func(h *hostState) { h.memAvailable = 64*gib/10 + boot }, 0, ""},
		{"minimum reserve on a small host", func(h *hostState) { h.memTotal = 8 * gib; h.memAvailable = 4*gib - 1 }, 0, "reserved"},
		{"disk reserve", func(h *hostState) { h.diskFree = 100 * gib }, 0, "free on the work dir"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := ok
			tt.change(&h)
			got := admit(chTestConfig(), h, false, boot, tt.pending)
			if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
				t.Fatalf("admit = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAdmissionClosedWhileGuestsAreDenied(t *testing.T) {
	h := hostState{load1: 1, cpus: 16, memTotal: 64 * gib, memAvailable: 40 * gib, diskFree: 500 * gib, diskTotal: 1000 * gib}
	if got := admit(chTestConfig(), h, true, 2*gib, 0); !strings.Contains(got, "denied memory") {
		t.Fatalf("admit = %q", got)
	}
}

func TestMemoryReserveFlags(t *testing.T) {
	c := chTestConfig()
	c.ch.memReservePercent, c.ch.memReserveMinGiB = 25, 1
	if got := memoryReserve(c, 64*gib); got != 16*gib {
		t.Fatalf("reserve = %d, want 16 GiB", got)
	}
	c.ch.memReserveMinGiB = 32
	if got := memoryReserve(c, 64*gib); got != 32*gib {
		t.Fatalf("reserve = %d, want the 32 GiB minimum", got)
	}
}

func TestMemoryDecision(t *testing.T) {
	now := time.Now()
	stats := func(availGiB int64, psi float64) guestStats {
		return guestStats{MemAvailableKiB: availGiB << 20, MemTotalKiB: 8 << 20, PSISomeAvg10: psi, ReceivedAt: now.Add(-time.Second)}
	}
	base := vmMemory{bootBytes: 2 * gib, maxBytes: 10 * gib, actualBytes: 4 * gib}
	roomy := hostMemory{available: 40 * gib, reserve: 6 * gib}
	tests := []struct {
		name   string
		vm     func(*vmMemory)
		host   hostMemory
		action memoryAction
		target int64
	}{
		{"psi plugs a step", func(v *vmMemory) { v.stats = stats(2, 6) }, roomy, memoryPlug, 5 * gib},
		{"low available plugs", func(v *vmMemory) { v.stats = stats(0, 0) }, roomy, memoryPlug, 5 * gib},
		{"plug capped at hotplug size", func(v *vmMemory) { v.stats = stats(0, 50); v.actualBytes = 10 * gib }, roomy, memoryHold, 10 * gib},
		{"host reserve denies", func(v *vmMemory) { v.stats = stats(0, 50) }, hostMemory{available: 7 * gib, reserve: 6 * gib, pending: 1}, memoryDenied, 4 * gib},
		{"pending memory denies", func(v *vmMemory) { v.stats = stats(0, 50) }, hostMemory{available: 10 * gib, reserve: 6 * gib, pending: 4 * gib}, memoryDenied, 4 * gib},
		{"plug that exactly fits", func(v *vmMemory) { v.stats = stats(0, 50) }, hostMemory{available: 7 * gib, reserve: 6 * gib}, memoryPlug, 5 * gib},
		{"stale stats freeze", func(v *vmMemory) { v.stats = stats(0, 50); v.stats.ReceivedAt = now.Add(-11 * time.Second) }, roomy, memoryHold, 4 * gib},
		{"no stats yet", func(v *vmMemory) {}, roomy, memoryHold, 4 * gib},
		{"waits for stats after resize", func(v *vmMemory) { v.stats = stats(0, 50); v.resizedAt = now }, roomy, memoryHold, 4 * gib},
		{"stats after resize count", func(v *vmMemory) { v.stats = stats(0, 50); v.resizedAt = now.Add(-2 * time.Second) }, roomy, memoryPlug, 5 * gib},
		{"calm long enough unplugs", func(v *vmMemory) { v.stats = stats(4, 0); v.calmSince = now.Add(-31 * time.Second) }, roomy, memoryUnplug, 3 * gib},
		{"calm too short holds", func(v *vmMemory) { v.stats = stats(4, 0); v.calmSince = now.Add(-20 * time.Second) }, roomy, memoryHold, 4 * gib},
		{"spare but some pressure holds", func(v *vmMemory) { v.stats = stats(4, 1); v.calmSince = now.Add(-time.Minute) }, roomy, memoryHold, 4 * gib},
		{"never below boot", func(v *vmMemory) { v.stats = stats(4, 0); v.actualBytes = 2 * gib; v.calmSince = now.Add(-time.Minute) }, roomy, memoryHold, 2 * gib},
		{"partial unplug floors at boot", func(v *vmMemory) {
			v.stats = stats(4, 0)
			v.actualBytes = 2*gib + 512*mib
			v.calmSince = now.Add(-time.Minute)
		}, roomy, memoryUnplug, 2 * gib},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vm := base
			tt.vm(&vm)
			action, target := memoryDecision(vm, tt.host, now)
			if action != tt.action || target != tt.target {
				t.Fatalf("decision = %s %d MiB, want %s %d MiB", action, target/mib, tt.action, tt.target/mib)
			}
		})
	}
}

func TestStatsTrackCalm(t *testing.T) {
	vm := &chVM{mem: vmMemory{bootBytes: 2 * gib}}
	if vm.pending() != 2*gib {
		t.Fatal("an unreported VM must reserve its boot memory")
	}
	t0 := time.Now()
	calm := guestStats{MemAvailableKiB: 4 << 20, MemTotalKiB: 8 << 20, ReceivedAt: t0}
	vm.recordStats(calm)
	if vm.pending() != 0 || !vm.mem.calmSince.Equal(t0) {
		t.Fatalf("pending = %d, calmSince = %v", vm.pending(), vm.mem.calmSince)
	}
	calm.ReceivedAt = t0.Add(2 * time.Second)
	vm.recordStats(calm)
	if !vm.mem.calmSince.Equal(t0) {
		t.Fatal("calm period restarted while still calm")
	}
	busy := calm
	busy.PSISomeAvg10, busy.ReceivedAt = 3, t0.Add(4*time.Second)
	vm.recordStats(busy)
	if !vm.mem.calmSince.IsZero() {
		t.Fatal("pressure did not end the calm period")
	}
	vm.mem.grant(8*gib, 9*gib, t0.Add(5*time.Second))
	busy.ReceivedAt = t0.Add(5 * time.Second)
	vm.recordStats(busy)
	if vm.pending() != gib {
		t.Fatal("stats from before the resize released the plug reservation")
	}
}

func TestPlugPendingUntilGuestShowsIt(t *testing.T) {
	t0 := time.Now()
	pressured := func(at time.Time, totalMiB int64) guestStats {
		return guestStats{MemAvailableKiB: 256 << 10, MemTotalKiB: totalMiB << 10, PSISomeAvg10: 30, ReceivedAt: at}
	}
	m := vmMemory{bootBytes: 2 * gib, maxBytes: 10 * gib, actualBytes: 4 * gib}
	m.observe(pressured(t0, 3900))
	m.grant(4*gib, 5*gib, t0.Add(time.Second))
	roomy := hostMemory{available: 40 * gib, reserve: 6 * gib}
	// Reported after the resize, but virtio-mem has not onlined the block yet.
	m.observe(pressured(t0.Add(3*time.Second), 3900))
	if m.pendingPlug != gib {
		t.Fatalf("pending = %d MiB, want the plug still pending", m.pendingPlug/mib)
	}
	if action, _ := memoryDecision(m, roomy, t0.Add(4*time.Second)); action != memoryHold {
		t.Fatalf("plugged again before the last plug showed: %s", action)
	}
	m.observe(pressured(t0.Add(5*time.Second), 3900+1024-100))
	if m.pendingPlug != 0 {
		t.Fatal("plug still pending after MemTotal grew by it")
	}
	if action, target := memoryDecision(m, roomy, t0.Add(6*time.Second)); action != memoryPlug || target != 5*gib {
		t.Fatalf("decision = %s %d MiB", action, target/mib)
	}
	// A plug that never shows stops counting after plugSettle.
	m.grant(5*gib, 6*gib, t0.Add(7*time.Second))
	m.observe(pressured(t0.Add(8*time.Second+plugSettle), 3900))
	if m.pendingPlug != 0 {
		t.Fatal("plug pending forever")
	}
}

// After an unplug the guest must stay calm for another unplugCalm: one step
// per period, not one per report.
func TestUnplugOncePerCalmPeriod(t *testing.T) {
	t0 := time.Now()
	calm := func(at time.Time) guestStats {
		return guestStats{MemAvailableKiB: 6 << 20, MemTotalKiB: 8 << 20, ReceivedAt: at}
	}
	roomy := hostMemory{available: 40 * gib, reserve: 6 * gib}
	m := vmMemory{bootBytes: 2 * gib, maxBytes: 10 * gib, actualBytes: 8 * gib}
	m.observe(calm(t0))
	m.observe(calm(t0.Add(31 * time.Second)))
	action, target := memoryDecision(m, roomy, t0.Add(31*time.Second))
	if action != memoryUnplug {
		t.Fatalf("decision = %s", action)
	}
	m.grant(m.actualBytes, target, t0.Add(31*time.Second))
	m.actualBytes = target
	m.observe(calm(t0.Add(33 * time.Second)))
	if action, _ := memoryDecision(m, roomy, t0.Add(33*time.Second)); action != memoryHold {
		t.Fatalf("unplugged again 2 s after an unplug: %s", action)
	}
	m.observe(calm(t0.Add(64 * time.Second)))
	if action, _ := memoryDecision(m, roomy, t0.Add(64*time.Second)); action != memoryUnplug {
		t.Fatalf("no unplug after another calm period: %s", action)
	}
}

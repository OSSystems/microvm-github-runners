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
		name      string
		change    func(*hostState)
		pending   int64
		committed int64
		want      string
	}{
		{"room", func(*hostState) {}, 0, 0, ""},
		{"load at the limit", func(h *hostState) { h.load1 = 16 }, 0, 0, "load"},
		// Reserve is max(10% of 64 GiB, 2 GiB) = 6.4 GiB; 6.4 + 2 boot + 12 pending > 20.
		{"pending boots count", func(*hostState) {}, 12 * gib, 0, "pending"},
		{"pending boots that still fit", func(*hostState) {}, 11 * gib / 2, 0, ""},
		{"percent reserve", func(h *hostState) { h.memAvailable = 8 * gib }, 0, 0, "reserved"},
		{"exactly enough", func(h *hostState) { h.memAvailable = 64*gib/10 + boot }, 0, 0, ""},
		{"minimum reserve on a small host", func(h *hostState) { h.memTotal = 8 * gib; h.memAvailable = 4*gib - 1 }, 0, 0, "reserved"},
		// 64 - 6.4 reserve - 54 committed leaves 3.6 GiB: room for a 2 GiB
		// boot and its next 1 GiB plug.
		{"room left by running VMs", func(h *hostState) { h.memAvailable = 60 * gib }, 0, 54 * gib, ""},
		// Running VMs were given 55 GiB but have not touched it yet.
		{"memory given to running VMs counts", func(h *hostState) { h.memAvailable = 60 * gib }, 0, 55 * gib, "committed"},
		{"disk reserve", func(h *hostState) { h.diskFree = 100 * gib }, 0, 0, "free on the work dir"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := ok
			tt.change(&h)
			got := admit(chTestConfig(), h, false, boot, tt.pending, tt.committed)
			if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
				t.Fatalf("admit = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAdmissionClosedWhileGuestsAreDenied(t *testing.T) {
	h := hostState{load1: 1, cpus: 16, memTotal: 64 * gib, memAvailable: 40 * gib, diskFree: 500 * gib, diskTotal: 1000 * gib}
	if got := admit(chTestConfig(), h, true, 2*gib, 0, 0); !strings.Contains(got, "denied memory") {
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

func TestStatsTrackCalm(t *testing.T) {
	vm := &chVM{mem: vmMemory{BootBytes: 2 * gib}}
	if vm.pending() != 2*gib {
		t.Fatal("an unreported VM must reserve its boot memory")
	}
	t0 := time.Now()
	calm := guestStats{MemAvailableKiB: 4 << 20, MemTotalKiB: 8 << 20, ReceivedAt: t0}
	vm.recordStats(calm)
	if vm.pending() != 0 || !vm.mem.CalmSince.Equal(t0) {
		t.Fatalf("pending = %d, calmSince = %v", vm.pending(), vm.mem.CalmSince)
	}
	calm.ReceivedAt = t0.Add(2 * time.Second)
	vm.recordStats(calm)
	if !vm.mem.CalmSince.Equal(t0) {
		t.Fatal("calm period restarted while still calm")
	}
	busy := calm
	busy.PSISomeAvg10, busy.ReceivedAt = 3, t0.Add(4*time.Second)
	vm.recordStats(busy)
	if !vm.mem.CalmSince.IsZero() {
		t.Fatal("pressure did not end the calm period")
	}
	vm.mem.Grant(8*gib, 9*gib, t0.Add(5*time.Second))
	busy.ReceivedAt = t0.Add(5 * time.Second)
	vm.recordStats(busy)
	if vm.pending() != gib {
		t.Fatal("stats from before the resize released the plug reservation")
	}
}

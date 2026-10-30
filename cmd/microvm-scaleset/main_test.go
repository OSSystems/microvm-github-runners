package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryRoom(t *testing.T) {
	tests := []struct {
		name      string
		running   int
		available float64
		want      int
	}{
		{"available memory limits launch batch", 0, 39, 3},
		{"overcommit permits eighth VM", 7, 39, 3},
		{"assigned memory still caps concurrency", 10, 39, 0},
		{"host reserve is retained", 7, 17, 0},
		{"exactly one VM fits above reserve", 7, 18, 1},
		{"less than host reserve", 7, 4, 0},
	}
	p := &pool{cfg: config{vmMemGiB: 10, memReserveGiB: 8, memCommitRatio: 2}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := p.memoryRoom(tt.running, map[string]float64{"MemTotal": 62.7, "MemAvailable": tt.available})
			if got != tt.want {
				t.Fatalf("room = %d, want %d (%s)", got, tt.want, reason)
			}
			if (got == 0) != (reason != "") {
				t.Fatalf("room = %d, reason = %q", got, reason)
			}
		})
	}
}

func TestRestartCountsExistingRunner(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\ncase \"$*\" in *ActiveState*) echo active;; *InvocationID*) echo boot-1;; *) exit 1;; esac\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	slot := "runner-1"
	slotDir := filepath.Join(dir, slot)
	if err := os.Mkdir(slotDir, 0700); err != nil {
		t.Fatal(err)
	}
	runner := "runner-1-12345678"
	metadata := map[string]any{"version": 1, "org": "example", "scaleSetId": 1, "runner": runner, "startedAt": time.Now().Add(-time.Minute).UTC(), "memoryGiB": 10}
	data, _ := json.Marshal(metadata)
	if err := os.WriteFile(filepath.Join(slotDir, "owner.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	ref, _ := json.Marshal(map[string]any{"AgentId": "7", "AgentName": runner, "RunnerScaleSetId": 1})
	jit, _ := json.Marshal(map[string]string{".runner": base64.StdEncoding.EncodeToString(ref), ".credentials": "not-a-real-credential"})
	if err := os.WriteFile(filepath.Join(slotDir, "jitconfig"), []byte(base64.StdEncoding.EncodeToString(jit)), 0400); err != nil {
		t.Fatal(err)
	}
	set := &scaleSet{org: "example", id: 1, assigned: 1, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p := &pool{cfg: config{jitDir: dir, vmMemGiB: 10, memReserveGiB: 1e9}, slots: []string{slot}, sets: []*scaleSet{set}, owners: map[string]owner{}, booting: map[string]time.Time{}, logger: set.logger}
	set.backend = p
	for i := 0; i < 2; i++ {
		if got := p.reconcile(context.Background())[set]; got != 1 {
			t.Fatalf("running after controller restart = %d, want existing runner counted once", got)
		}
		if _, err := os.Stat(filepath.Join(slotDir, "jitconfig")); err != nil {
			t.Fatalf("active runner credentials changed: %v", err)
		}
	}
}

func TestMemoryFlags(t *testing.T) {
	for _, args := range [][]string{{"--backend=qemu", "--slots=a", "--mem-overcommit=1"}, {"--backend=qemu", "--slots=a", "--mem-commit-ratio=2"}} {
		c, err := parseConfig(args)
		if err != nil {
			t.Fatal(err)
		}
		if c.memCommitRatio != 2 {
			t.Fatalf("ratio = %v, want 2", c.memCommitRatio)
		}
	}
	for _, args := range [][]string{{"--backend=qemu", "--slots=a", "--mem-overcommit=1", "--mem-commit-ratio=2"}, {"--backend=qemu", "--slots=a", "--mem-commit-ratio=NaN"}, {"--backend=qemu", "--slots=a", "--vm-mem=0"}, {"--backend=qemu", "--slots=a", "--mem-reserve=-1"}, {"--backend=qemu", "--slots=a", "--balloon-enable", "--vm-mem=4", "--balloon-floor=8"}} {
		if _, err := parseConfig(args); err == nil {
			t.Fatalf("accepted invalid settings %v", args)
		}
	}
}

func TestBalloonSafety(t *testing.T) {
	now := time.Now()
	c := config{vmMemGiB: 10, memReserveGiB: 8, balloonEnabled: true, balloonFloorGiB: 8, balloonHeadroomGiB: 2}
	owner := ownerRecord{StartedAt: now.Add(-time.Hour)}
	sample := vmSample{ObservedAt: now, FreshSamples: 3, Guest: guestMemory{ActualBytes: 10 * gib, CeilingBytes: 10 * gib, AvailableBytes: 7 * gib, OOMKills: 0, UpdatedAt: now.Unix()}}
	tests := []struct {
		name      string
		change    func(*config, *vmSample, *ownerRecord)
		available float64
		want      int64
	}{
		{"bounded pressure reclaim", func(*config, *vmSample, *ownerRecord) {}, 12, 10*gib - (256 << 20)},
		{"stale stats freeze", func(_ *config, s *vmSample, _ *ownerRecord) { s.Guest.UpdatedAt = now.Add(-time.Minute).Unix() }, 12, 10 * gib},
		{"future stats freeze", func(_ *config, s *vmSample, _ *ownerRecord) { s.Guest.UpdatedAt = now.Add(time.Minute).Unix() }, 12, 10 * gib},
		{"missing stats freeze", func(_ *config, s *vmSample, _ *ownerRecord) { s.Guest.AvailableBytes = -1 }, 12, 10 * gib},
		{"startup grace", func(_ *config, _ *vmSample, r *ownerRecord) { r.StartedAt = now }, 12, 10 * gib},
		{"three samples required", func(_ *config, s *vmSample, _ *ownerRecord) { s.FreshSamples = 2 }, 12, 10 * gib},
		{"resize cooldown", func(_ *config, s *vmSample, _ *ownerRecord) { s.ResizedAt = now }, 12, 10 * gib},
		{"floor retained", func(_ *config, s *vmSample, _ *ownerRecord) { s.Guest.ActualBytes = 8 * gib }, 12, 8 * gib},
		{"keep guest headroom", func(_ *config, s *vmSample, _ *ownerRecord) { s.Guest.AvailableBytes = 2 * gib }, 12, 10 * gib},
		{"grow a needy guest", func(_ *config, s *vmSample, _ *ownerRecord) {
			s.Guest.ActualBytes = 8 * gib
			s.Guest.AvailableBytes = gib
		}, 12, 8*gib + (512 << 20)},
		{"growth denied below reserve", func(_ *config, s *vmSample, _ *ownerRecord) {
			s.Guest.ActualBytes = 8 * gib
			s.Guest.AvailableBytes = gib
		}, 7, 8 * gib},
		{"bounded rollback growth", func(c *config, s *vmSample, _ *ownerRecord) { c.balloonEnabled = false; s.Guest.ActualBytes = 8 * gib }, 12, 8*gib + (512 << 20)},
		{"stale rollback stays frozen", func(c *config, s *vmSample, _ *ownerRecord) {
			c.balloonEnabled = false
			s.Guest.ActualBytes = 8 * gib
			s.Guest.UpdatedAt = 0
		}, 12, 8 * gib},
		{"rollback retains host reserve", func(c *config, s *vmSample, _ *ownerRecord) { c.balloonEnabled = false; s.Guest.ActualBytes = 8 * gib }, 8, 8 * gib},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, s, r := c, sample, owner
			tt.change(&cfg, &s, &r)
			if got := balloonTarget(cfg, r, s, tt.available, true, now); got != tt.want {
				t.Fatalf("target = %.3f GiB, want %.3f", float64(got)/float64(gib), float64(tt.want)/float64(gib))
			}
		})
	}
}

func TestPendingVMGrowthIsReserved(t *testing.T) {
	now := time.Now()
	p := &pool{cfg: config{balloonHeadroomGiB: 2}, owners: map[string]owner{"one": {runner: "one-a", record: ownerRecord{StartedAt: now, MemoryGiB: 10}}}}
	if got, _ := p.growthReservation(now); got != 10 {
		t.Fatalf("unsampled reservation=%v, want10", got)
	}
	p.samples = map[string]vmSample{"one": {Runner: "one-a", ObservedAt: now, RAMBytes: 2 * gib, Guest: guestMemory{ActualBytes: 8 * gib, CeilingBytes: 10 * gib, AvailableBytes: 5 * gib, OOMKills: 0, UpdatedAt: now.Unix()}}}
	if got, _ := p.growthReservation(now); got != 8 {
		t.Fatalf("young reservation=%v, want8", got)
	}
	o := p.owners["one"]
	o.record.StartedAt = now.Add(-time.Hour)
	p.owners["one"] = o
	if got, _ := p.growthReservation(now); got != 2 {
		t.Fatalf("mature reservation=%v, want2", got)
	}
	s := p.samples["one"]
	s.Guest.UpdatedAt = 0
	p.samples["one"] = s
	if got, _ := p.growthReservation(now); got != 10 {
		t.Fatalf("stale reservation=%v, want10", got)
	}
}

func TestQueuedStartStillOccupiesSlot(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
case "$*" in *ActiveState*) echo inactive;; *Job*) echo 2048;; *) exit 1;; esac
`
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if !vmRunning("runner-1") {
		t.Fatal("queued systemd start was treated as a free slot")
	}
}

func TestBalloonDoesNotChurnAtPressureBoundary(t *testing.T) {
	now := time.Now()
	c := config{vmMemGiB: 10, memReserveGiB: 8, balloonEnabled: true, balloonFloorGiB: 8, balloonHeadroomGiB: 2}
	r := ownerRecord{StartedAt: now.Add(-time.Hour)}
	s := vmSample{ObservedAt: now, FreshSamples: 4, Guest: guestMemory{ActualBytes: 9 * gib, CeilingBytes: 10 * gib, AvailableBytes: 6 * gib, OOMKills: 0, UpdatedAt: now.Unix()}}
	if got := balloonTarget(c, r, s, 18.25, true, now); got != 9*gib {
		t.Fatalf("boundary caused growth to %.2f GiB, want steady 9 GiB", float64(got)/float64(gib))
	}
}

func TestElasticAdmissionBudgetsAllocationInsteadOfCeiling(t *testing.T) {
	now := time.Now()
	p := &pool{cfg: config{jitDir: t.TempDir(), vmMemGiB: 16, vmInitialMemGiB: 4, memReserveGiB: 8, memCommitRatio: 2, balloonHeadroomGiB: 1}, owners: map[string]owner{}, samples: map[string]vmSample{}}
	for i := 0; i < 6; i++ {
		slot := fmt.Sprintf("slot-%d", i)
		r := ownerRecord{Runner: slot, MemoryGiB: 16, InitialMemoryGiB: 4, StartedAt: now.Add(-time.Minute)}
		p.owners[slot] = owner{runner: slot, record: r}
		if err := os.MkdirAll(filepath.Join(p.cfg.jitDir, slot, "control"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p.cfg.jitDir, slot, "control", "ready"), []byte(slot), 0644); err != nil {
			t.Fatal(err)
		}
		p.samples[slot] = vmSample{Runner: slot, ObservedAt: now, RAMBytes: 4 * gib, TargetBytes: 4 * gib, Guest: guestMemory{ActualBytes: 4 * gib, CeilingBytes: 16 * gib, AvailableBytes: 2 * gib, UpdatedAt: now.Unix(), OOMKills: 0}}
	}
	if n, reason := p.memoryRoom(6, map[string]float64{"MemTotal": 64, "MemAvailable": 40}); n != 8 {
		t.Fatalf("room=%d (%s), want 8 additional starts", n, reason)
	}
	os.Remove(filepath.Join(p.cfg.jitDir, "slot-0", "control", "ready"))
	if n, reason := p.memoryRoom(6, map[string]float64{"MemTotal": 64, "MemAvailable": 40}); n != 7 {
		t.Fatalf("parallel startup room=%d (%s), want 7 with 4 GiB pending reservation", n, reason)
	}
}

func TestElasticGuestDoesNotExpandJustBecauseHostIsIdle(t *testing.T) {
	now := time.Now()
	c := config{vmMemGiB: 16, vmInitialMemGiB: 4, balloonEnabled: true, balloonFloorGiB: 3, balloonHeadroomGiB: 1, memReserveGiB: 8}
	r := ownerRecord{MemoryGiB: 16, InitialMemoryGiB: 4, StartedAt: now.Add(-time.Hour)}
	s := vmSample{ObservedAt: now, Guest: guestMemory{ActualBytes: 4 * gib, CeilingBytes: 16 * gib, AvailableBytes: 2 * gib, UpdatedAt: now.Unix(), OOMKills: 0}}
	if got := balloonTarget(c, r, s, 50, false, now); got != 4*gib {
		t.Fatalf("idle host expanded light guest to %d", got)
	}
	s.Runner = "runner"
	s.RAMBytes = gib
	growth, urgent := slotGrowthReservation(c, owner{runner: "runner", record: r}, s, now)
	if growth != 3 || urgent {
		t.Fatalf("growth=%v urgent=%v, want 3 GiB of allocated but unfaulted RAM", growth, urgent)
	}
}

func TestBalloonWaitsForStatsAfterResize(t *testing.T) {
	now := time.Now()
	c := config{vmMemGiB: 16, vmInitialMemGiB: 2, memReserveGiB: 8, balloonEnabled: true, balloonFloorGiB: 2, balloonHeadroomGiB: 1}
	r := ownerRecord{MemoryGiB: 16, InitialMemoryGiB: 2, StartedAt: now.Add(-time.Hour)}
	s := vmSample{ObservedAt: now, ResizedAt: now.Add(-time.Second), Guest: guestMemory{ActualBytes: 3 * gib, CeilingBytes: 16 * gib, AvailableBytes: gib / 2, UpdatedAt: now.Add(-2 * time.Second).Unix(), OOMKills: 0}}
	if got := balloonTarget(c, r, s, 40, true, now); got != 3*gib {
		t.Fatalf("reused pre-resize stats to grant %d bytes", got)
	}
}

func TestQEMUBackendNeedsSlots(t *testing.T) {
	if _, err := parseConfig([]string{"--backend=qemu"}); err == nil || !strings.Contains(err.Error(), "--slots") {
		t.Fatalf("parseConfig = %v, want --slots required", err)
	}
}

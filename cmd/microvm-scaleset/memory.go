package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type vmSample struct {
	Runner            string      `json:"runner"`
	Invocation        string      `json:"invocation"`
	ObservedAt        time.Time   `json:"observedAt"`
	Guest             guestMemory `json:"guest"`
	ResidentBytes     int64       `json:"residentBytes"`
	RAMBytes          int64       `json:"ramBytes"`
	SwapBytes         int64       `json:"swapBytes"`
	TargetBytes       int64       `json:"targetBytes"`
	ResizedAt         time.Time   `json:"resizedAt,omitempty"`
	FreshSamples      int         `json:"freshSamples"`
	Pinned            bool        `json:"pinnedAtCeiling"`
	Error             string      `json:"error,omitempty"`
	PeakUsedBytes     int64       `json:"peakObservedGuestUsedBytes"`
	PeakResidentBytes int64       `json:"peakObservedResidentBytes"`
	Observations      int         `json:"observations"`
}

type unitInfo struct{ State, Invocation, Cgroup string }

func readUnit(ctx context.Context, slot string) (unitInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "systemctl", "show", "--property=ActiveState,InvocationID,ControlGroup", "microvm@"+slot+".service").Output()
	var u unitInfo
	if err != nil {
		return u, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "ActiveState":
			u.State = value
		case "InvocationID":
			u.Invocation = value
		case "ControlGroup":
			u.Cgroup = value
		}
	}
	return u, nil
}

func cgroupValue(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func (p *pool) memorySnapshot() map[string]vmSample {
	p.memoryMu.Lock()
	defer p.memoryMu.Unlock()
	result := make(map[string]vmSample, len(p.samples))
	for k, v := range p.samples {
		result[k] = v
	}
	return result
}

func (p *pool) telemetryLoop(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		p.sampleMemory(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (p *pool) sampleMemory(ctx context.Context) {
	p.mu.Lock()
	owners := make(map[string]owner, len(p.owners))
	for slot, o := range p.owners {
		owners[slot] = o
	}
	slots := append([]string(nil), p.slots...)
	waiting := false
	for _, s := range p.sets {
		waiting = waiting || s.waiting > 0
	}
	p.mu.Unlock()
	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)
	for _, slot := range slots {
		select {
		case <-ctx.Done():
			return
		case limit <- struct{}{}:
		}
		wg.Add(1)
		go func(slot string) {
			defer wg.Done()
			defer func() { <-limit }()
			p.sampleSlot(ctx, slot, owners[slot], waiting)
		}(slot)
	}
	wg.Wait()
}

func (p *pool) sampleSlot(ctx context.Context, slot string, o owner, waiting bool) {
	unit, err := readUnit(ctx, slot)
	if err != nil || unit.State != "active" || unit.Invocation == "" {
		return
	}
	ready, initErr := p.initializeMemory(ctx, slot, o, unit)
	if initErr != nil || !ready {
		return
	}
	sample := vmSample{Runner: o.runner, Invocation: unit.Invocation, ObservedAt: time.Now(), RAMBytes: -1, ResidentBytes: -1, SwapBytes: -1}
	if strings.HasPrefix(unit.Cgroup, "/") && !strings.Contains(unit.Cgroup, "..") {
		base := filepath.Join("/sys/fs/cgroup", unit.Cgroup)
		sample.ResidentBytes = cgroupValue(filepath.Join(base, "memory.current"))
		sample.SwapBytes = cgroupValue(filepath.Join(base, "memory.swap.current"))
		if raw, e := os.ReadFile(filepath.Join(base, "memory.stat")); e == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 2 && fields[0] == "shmem" {
					sample.RAMBytes, _ = strconv.ParseInt(fields[1], 10, 64)
				}
			}
		}
	}
	q, err := openQMP(ctx, p.socketPath(slot))
	if err == nil {
		sample.Guest, err = q.memory()
		q.Close()
	}
	if err != nil {
		sample.Error = err.Error()
	}
	p.memoryMu.Lock()
	previous, existed := p.samples[slot]
	same := existed && previous.Runner == sample.Runner && previous.Invocation == sample.Invocation
	if same {
		sample.PeakUsedBytes = previous.PeakUsedBytes
		sample.PeakResidentBytes = previous.PeakResidentBytes
		sample.Observations = previous.Observations
		sample.TargetBytes = previous.TargetBytes
		sample.ResizedAt = previous.ResizedAt
		sample.Pinned = previous.Pinned
		if sample.Error == "" && sample.Guest.fresh(time.Now()) {
			sample.FreshSamples = previous.FreshSamples
			if sample.Guest.UpdatedAt > previous.Guest.UpdatedAt {
				sample.FreshSamples++
			}
		}
	}
	if sample.TargetBytes == 0 {
		sample.TargetBytes = sample.Guest.ActualBytes
	}
	if sample.Error == "" && (sample.Guest.OOMKills > 0 || (same && previous.Error == "" && previous.TargetBytes > 0 && previous.Guest.ActualBytes > 0 && previous.Guest.ActualBytes <= previous.TargetBytes+(64<<20) && sample.Guest.ActualBytes > previous.TargetBytes+(64<<20) && previous.TargetBytes < sample.Guest.CeilingBytes)) {
		if !sample.Pinned {
			p.logger.Warn("guest memory emergency; reclaim disabled for this boot", "slot", slot, "oomKills", sample.Guest.OOMKills)
		}
		sample.Pinned = true
	}
	if sample.Guest.fresh(time.Now()) && sample.Error == "" {
		sample.PeakUsedBytes = max(sample.PeakUsedBytes, sample.Guest.ActualBytes-sample.Guest.AvailableBytes)
		sample.Observations++
	}
	sample.PeakResidentBytes = max(sample.PeakResidentBytes, sample.ResidentBytes)
	if p.samples == nil {
		p.samples = map[string]vmSample{}
	}
	p.samples[slot] = sample
	p.memoryMu.Unlock()
	if o.set != nil && sample.Error == "" {
		p.controlMemory(ctx, slot, o, sample, waiting)
	}
}

// balloonTarget never spends unobserved host headroom or acts on stale guest data.
func balloonTarget(c config, r ownerRecord, s vmSample, hostAvailable float64, waiting bool, now time.Time) int64 {
	actual := s.Guest.ActualBytes
	if s.Error != "" || !s.Guest.fresh(now) || s.ObservedAt.After(now) || now.Sub(s.ObservedAt) > 15*time.Second {
		return actual
	}
	if !s.ResizedAt.IsZero() && s.Guest.UpdatedAt <= s.ResizedAt.Unix() {
		return actual
	}
	ceiling := s.Guest.CeilingBytes
	headroom := int64(c.balloonHeadroomGiB * float64(gib))
	floor := min(ceiling, int64(c.balloonFloorGiB*float64(gib)))
	bootMemory := c.vmInitialMemGiB
	if bootMemory <= 0 {
		bootMemory = c.vmMemGiB
	}
	elastic := r.InitialMemoryGiB > 0 && r.InitialMemoryGiB < r.MemoryGiB
	pressure := hostAvailable < c.memReserveGiB+bootMemory && (waiting || hostAvailable < c.memReserveGiB)
	target := actual
	if !c.balloonEnabled || s.Pinned {
		target = ceiling
	} else if s.Guest.AvailableBytes < headroom {
		target = min(ceiling, actual+2*headroom-s.Guest.AvailableBytes)
	} else if !pressure {
		if !elastic && (!waiting || hostAvailable >= c.memReserveGiB+bootMemory+c.balloonHeadroomGiB) {
			target = ceiling
		}
	} else if now.Sub(r.StartedAt) >= 90*time.Second && s.FreshSamples >= 3 && now.Sub(s.ResizedAt) >= 30*time.Second && s.Guest.UpdatedAt > s.ResizedAt.Unix()+5 {
		target = max(floor, min(ceiling, actual-s.Guest.AvailableBytes+headroom))
		target = max(target, actual-(256<<20))
	}
	if target > actual {
		budget := int64(max(0, hostAvailable-c.memReserveGiB) * float64(gib))
		step := int64(512 << 20)
		if elastic {
			step = 2 * gib
		}
		target = min(target, actual+min(step, budget))
		target = target / (1 << 20) * (1 << 20)
	} else if target < actual {
		target = (target + (1 << 20) - 1) / (1 << 20) * (1 << 20)
	}
	if target > actual-(64<<20) && target < actual+(64<<20) {
		return actual
	}
	return target
}

func (p *pool) controlMemory(ctx context.Context, slot string, o owner, s vmSample, waiting bool) {
	// Serialize grants: a QMP acknowledgement can precede actual RAM allocation.
	p.controlMu.Lock()
	defer p.controlMu.Unlock()
	mem, err := meminfoGiB()
	if err != nil {
		return
	}
	// Keep headroom already promised to other running or pending VMs.
	samples := p.memorySnapshot()
	p.mu.Lock()
	otherGrowth := 0.0
	for other, current := range p.owners {
		if other != slot {
			reserve, _ := slotGrowthReservation(p.cfg, current, samples[other], time.Now())
			otherGrowth += reserve
		}
	}
	ownerNow, exists := p.owners[slot]
	stillOwned := exists && ownerNow.runner == o.runner && !p.unresolvedOwners
	p.mu.Unlock()
	if !stillOwned {
		return
	}
	available := mem["MemAvailable"] - otherGrowth
	if o.record.InitialMemoryGiB > 0 && o.record.InitialMemoryGiB < o.record.MemoryGiB {
		ownGrowth, _ := slotGrowthReservation(p.cfg, o, s, time.Now())
		available -= ownGrowth
	}
	policy := p.cfg
	if policy.balloonSlot != "" && policy.balloonSlot != slot {
		policy.balloonEnabled = false
	}
	target := balloonTarget(policy, o.record, s, available, waiting, time.Now())
	if target == s.Guest.ActualBytes || s.TargetBytes > s.Guest.ActualBytes {
		return
	}
	q, err := openQMP(ctx, p.socketPath(slot))
	if err != nil {
		return
	}
	defer q.Close()
	unit, err := readUnit(ctx, slot)
	if err != nil || unit.State != "active" || unit.Invocation != s.Invocation {
		return
	}
	p.mu.Lock()
	current, ok := p.owners[slot]
	same := ok && current.runner == o.runner && !p.unresolvedOwners
	p.mu.Unlock()
	if !same {
		return
	}
	if err = q.setMemory(target); err != nil {
		p.logger.Warn("balloon request failed", "slot", slot, "error", err)
		p.memoryMu.Lock()
		latest := p.samples[slot]
		latest.FreshSamples = 0
		latest.ResizedAt = time.Now()
		p.samples[slot] = latest
		p.memoryMu.Unlock()
		return
	}
	p.memoryMu.Lock()
	latest := p.samples[slot]
	if latest.Invocation == s.Invocation && latest.Runner == o.runner {
		latest.TargetBytes = target
		latest.ResizedAt = time.Now()
		latest.FreshSamples = 0
		p.samples[slot] = latest
	}
	p.memoryMu.Unlock()
	p.logger.Info("guest memory target changed", "slot", slot, "org", o.set.org, "fromGiB", float64(s.Guest.ActualBytes)/float64(gib), "toGiB", float64(target)/float64(gib), "guestAvailableGiB", float64(s.Guest.AvailableBytes)/float64(gib))
}

func slotGrowthReservation(c config, o owner, s vmSample, now time.Time) (float64, bool) {
	ceiling := o.record.MemoryGiB
	elastic := o.record.InitialMemoryGiB > 0 && o.record.InitialMemoryGiB < ceiling
	fresh := s.Runner == o.runner && s.Error == "" && s.Guest.fresh(now) && now.Sub(s.ObservedAt) <= 15*time.Second
	if elastic {
		if !fresh {
			return o.record.InitialMemoryGiB, true
		}
		allocated := max(s.Guest.ActualBytes, s.TargetBytes)
		return float64(max(int64(0), allocated-max(int64(0), s.RAMBytes))) / float64(gib), s.Pinned && s.Guest.AvailableBytes < int64(c.balloonHeadroomGiB*float64(gib))
	}
	if !fresh || now.Sub(o.record.StartedAt) < bootGrace {
		resident := 0.0
		if fresh && s.RAMBytes >= 0 {
			resident = float64(s.RAMBytes) / float64(gib)
		}
		return max(0, ceiling-resident), false
	}
	growth := min(max(0, ceiling-float64(s.Guest.ActualBytes)/float64(gib)), c.balloonHeadroomGiB)
	growth = max(growth, float64(max(int64(0), s.TargetBytes-s.Guest.ActualBytes))/float64(gib))
	emergency := s.Pinned && (s.Guest.ActualBytes < s.Guest.CeilingBytes || s.Guest.AvailableBytes < int64(c.balloonHeadroomGiB*float64(gib)))
	return growth, emergency
}

func (p *pool) growthReservation(now time.Time) (float64, bool) {
	samples := p.memorySnapshot()
	reserve := 0.0
	emergency := false
	for slot, o := range p.owners {
		growth, urgent := slotGrowthReservation(p.cfg, o, samples[slot], now)
		reserve += growth
		emergency = emergency || urgent
	}
	return reserve, emergency
}

func (p *pool) writeStatus(running map[*scaleSet]int, unknown int) {
	if p.cfg.statusFile == "" {
		return
	}
	mem, err := meminfoGiB()
	if err != nil {
		return
	}
	type slotStatus struct {
		Slot   string       `json:"slot"`
		Owner  *ownerRecord `json:"owner,omitempty"`
		Source string       `json:"source"`
		Memory *vmSample    `json:"memory,omitempty"`
	}
	type orgStatus struct{ Assigned, Running, Waiting int }
	status := struct {
		UpdatedAt     time.Time            `json:"updatedAt"`
		Host          map[string]float64   `json:"hostGiB"`
		CommitRatio   float64              `json:"commitRatio"`
		CommitCapGiB  float64              `json:"commitCapGiB"`
		NominalGiB    float64              `json:"nominalGiB"`
		Gate          string               `json:"gate"`
		Unknown       int                  `json:"unknownOwners"`
		Organizations map[string]orgStatus `json:"organizations"`
		Slots         []slotStatus         `json:"slots"`
	}{UpdatedAt: time.Now(), Host: map[string]float64{"total": mem["MemTotal"], "available": mem["MemAvailable"], "reserve": p.cfg.memReserveGiB}, CommitRatio: p.cfg.memCommitRatio, CommitCapGiB: (mem["MemTotal"] - p.cfg.memReserveGiB) * p.cfg.memCommitRatio, Gate: p.busyReason, Unknown: unknown, Organizations: map[string]orgStatus{}}
	samples := p.memorySnapshot()
	for _, set := range p.sets {
		status.Organizations[set.org] = orgStatus{set.assigned, running[set], max(0, set.assigned-running[set])}
	}
	for _, slot := range p.slots {
		entry := slotStatus{Slot: slot, Source: "free"}
		if o, ok := p.owners[slot]; ok {
			r := o.record
			entry.Owner = &r
			entry.Source = o.source
			status.NominalGiB += committedMemoryGiB(o, samples[slot])
		}
		if sample, ok := samples[slot]; ok && time.Since(sample.ObservedAt) < 15*time.Second && ((entry.Owner != nil && sample.Runner == entry.Owner.Runner) || (entry.Owner == nil && sample.Runner == "")) {
			entry.Memory = &sample
			if entry.Owner == nil {
				entry.Source = "unknown"
			}
		}
		status.Slots = append(status.Slots, entry)
	}
	raw, err := json.MarshalIndent(status, "", "  ")
	if err == nil {
		err = atomicWrite(p.cfg.statusFile, raw, 0640)
	}
	if err != nil {
		p.logger.Warn("writing pool status", "error", fmt.Sprint(err))
	}
}

func committedMemoryGiB(o owner, s vmSample) float64 {
	if o.record.InitialMemoryGiB > 0 && o.record.InitialMemoryGiB < o.record.MemoryGiB {
		if s.Runner == o.runner && s.Guest.ActualBytes > 0 {
			return float64(max(s.Guest.ActualBytes, s.TargetBytes)) / float64(gib)
		}
		return o.record.InitialMemoryGiB
	}
	return o.record.MemoryGiB
}

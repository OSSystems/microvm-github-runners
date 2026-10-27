package main

import (
	"context"
	"errors"
	"path/filepath"
)

func (p *pool) initialMemoryReady(slot, runner string) bool {
	marker, err := readBounded(filepath.Join(p.cfg.jitDir, slot, "control", "ready"), 4096)
	return err == nil && string(marker) == runner
}

// initializeMemory keeps the guest runner gated until its startup allocation is
// observed. A balloon command's acknowledgement alone does not release the gate.
func (p *pool) initializeMemory(ctx context.Context, slot string, o owner, unit unitInfo) (bool, error) {
	if o.record.InitialMemoryGiB <= 0 || o.record.InitialMemoryGiB >= o.record.MemoryGiB {
		return true, nil
	}
	if p.initialMemoryReady(slot, o.runner) {
		return true, nil
	}
	p.controlMu.Lock()
	defer p.controlMu.Unlock()
	p.mu.Lock()
	currentOwner, owned := p.owners[slot]
	valid := owned && currentOwner.runner == o.runner && !p.unresolvedOwners
	p.mu.Unlock()
	if !valid {
		return false, errors.New("VM ownership unresolved during startup allocation")
	}
	before, err := readUnit(ctx, slot)
	if err != nil || before.Invocation != unit.Invocation || before.State != "active" {
		return false, errors.New("VM changed before startup allocation")
	}
	q, err := openQMP(ctx, p.socketPath(slot))
	if err != nil {
		return false, err
	}
	defer q.Close()
	var balloon struct {
		Actual int64 `json:"actual"`
	}
	if err := q.command("query-balloon", nil, &balloon); err != nil {
		return false, err
	}
	initial := int64(o.record.InitialMemoryGiB * float64(gib))
	if balloon.Actual > initial+64*1024*1024 {
		return false, q.setMemory(initial)
	}
	if balloon.Actual <= 0 {
		return false, errors.New("invalid startup balloon allocation")
	}
	currentUnit, err := readUnit(ctx, slot)
	if err != nil {
		return false, err
	}
	if unit.Invocation == "" || currentUnit.Invocation != unit.Invocation || currentUnit.State != "active" {
		return false, errors.New("VM changed during startup allocation")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	current, ok := p.owners[slot]
	if !ok || current.runner != o.runner {
		return false, errors.New("VM owner changed during startup allocation")
	}
	if err := atomicWrite(filepath.Join(p.cfg.jitDir, slot, "control", "ready"), []byte(o.runner), 0644); err != nil {
		return false, err
	}
	p.logger.Info("initial guest memory confirmed", "slot", slot, "runner", o.runner, "initialGiB", o.record.InitialMemoryGiB)
	return true, nil
}

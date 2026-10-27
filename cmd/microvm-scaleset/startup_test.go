package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestInitialMemoryMarkerMatchesRunnerExactly(t *testing.T) {
	p := testPool(t)
	slot, runner := "runner-1", "runner-1-current"
	dir := filepath.Join(p.cfg.jitDir, slot, "control")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"", "runner-1-previous", runner + "\n", runner} {
		if err := os.WriteFile(filepath.Join(dir, "ready"), []byte(marker), 0644); err != nil {
			t.Fatal(err)
		}
		if got := p.initialMemoryReady(slot, runner); got != (marker == runner) {
			t.Fatalf("marker %q: ready=%v", marker, got)
		}
	}
}

func TestStartupWaitsForObservedAllocation(t *testing.T) {
	p := testPool(t)
	dir, err := os.MkdirTemp("/tmp", "startup-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	p.cfg.stateDir = dir
	slot, runner := "runner-1", "runner-1-current"
	control := filepath.Join(p.cfg.jitDir, slot, "control")
	if err := os.MkdirAll(control, 0755); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nprintf 'ActiveState=active\\nInvocationID=boot-1\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	o := owner{runner: runner, record: ownerRecord{Runner: runner, MemoryGiB: 16, InitialMemoryGiB: 2}}
	p.owners[slot] = o
	var actual atomic.Int64
	var target atomic.Int64
	actual.Store(16 * gib)
	fakeQMP(t, p.socketPath(slot), func(execute string, args map[string]json.RawMessage) any {
		switch execute {
		case "query-balloon":
			return map[string]any{"actual": actual.Load()}
		case "balloon":
			var value int64
			if err := json.Unmarshal(args["value"], &value); err != nil {
				panic(err)
			}
			target.Store(value)
		}
		return map[string]any{}
	})
	unit := unitInfo{State: "active", Invocation: "boot-1"}
	ready, err := p.initializeMemory(context.Background(), slot, o, unit)
	if err != nil || ready || p.initialMemoryReady(slot, runner) || target.Load() != 2*gib {
		t.Fatalf("request released runner prematurely: ready=%v target=%d err=%v", ready, target.Load(), err)
	}
	actual.Store(2 * gib)
	ready, err = p.initializeMemory(context.Background(), slot, o, unit)
	if err != nil || !ready || !p.initialMemoryReady(slot, runner) {
		t.Fatalf("confirmed allocation did not release runner: ready=%v err=%v", ready, err)
	}
	info, err := os.Stat(filepath.Join(control, "ready"))
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("ready marker mode: info=%v err=%v", info, err)
	}
}

func TestStartupRejectsChangedInvocation(t *testing.T) {
	p := testPool(t)
	dir, err := os.MkdirTemp("/tmp", "startup-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	p.cfg.stateDir = dir
	slot, runner := "runner-1", "runner-1-current"
	if err := os.MkdirAll(filepath.Join(p.cfg.jitDir, slot, "control"), 0755); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nprintf 'ActiveState=active\\nInvocationID=replacement\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	o := owner{runner: runner, record: ownerRecord{Runner: runner, MemoryGiB: 16, InitialMemoryGiB: 2}}
	p.owners[slot] = o
	fakeQMP(t, p.socketPath(slot), func(execute string, _ map[string]json.RawMessage) any {
		if execute == "query-balloon" {
			return map[string]any{"actual": 2 * gib}
		}
		return map[string]any{}
	})
	ready, err := p.initializeMemory(context.Background(), slot, o, unitInfo{Invocation: "previous"})
	if ready || err == nil || p.initialMemoryReady(slot, runner) {
		t.Fatalf("changed boot released runner: ready=%v err=%v", ready, err)
	}
}

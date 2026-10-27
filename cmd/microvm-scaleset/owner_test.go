package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

type testRunnerClient struct {
	generate func(*scaleset.RunnerScaleSetJitRunnerSetting) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	lookup   func(string) (*scaleset.RunnerReference, error)
	remove   func(int64) error
}

func (c testRunnerClient) GenerateJitRunnerConfig(_ context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	return c.generate(s)
}
func (c testRunnerClient) GetRunnerByName(_ context.Context, name string) (*scaleset.RunnerReference, error) {
	return c.lookup(name)
}
func (c testRunnerClient) RemoveRunner(_ context.Context, id int64) error { return c.remove(id) }

func testPool(t *testing.T) *pool {
	t.Helper()
	return &pool{cfg: config{jitDir: t.TempDir(), vmMemGiB: 10, memCommitRatio: 2, maxLoadPerCPU: 1e6, balloonHeadroomGiB: 2}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), owners: map[string]owner{}, booting: map[string]time.Time{}, ownerRetry: map[string]time.Time{}, samples: map[string]vmSample{}}
}
func testIdentity(t *testing.T, p *pool, slot, name string) {
	t.Helper()
	dir := filepath.Join(p.cfg.jitDir, slot)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"AgentId": "7", "AgentName": name, "RunnerScaleSetId": "1"})
	outer, _ := json.Marshal(map[string]string{".runner": base64.StdEncoding.EncodeToString(raw), ".credentials": "secret-marker"})
	if err := os.WriteFile(filepath.Join(dir, "jitconfig"), []byte(base64.StdEncoding.EncodeToString(outer)), 0400); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownRunnerInhibitsBothOrganizations(t *testing.T) {
	p := testPool(t)
	p.slots = []string{"runner-1", "runner-2"}
	for _, slot := range p.slots {
		if err := os.Mkdir(filepath.Join(p.cfg.jitDir, slot), 0700); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in *ActiveState*runner-1.service*) echo active;; *ActiveState*) echo inactive;; *Job*) echo '';; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	boots := 0
	client := testRunnerClient{generate: func(*scaleset.RunnerScaleSetJitRunnerSetting) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
		boots++
		return nil, errors.New("unexpected boot")
	}}
	a := &scaleSet{org: "a", id: 1, assigned: 1, client: client, logger: p.logger}
	b := &scaleSet{org: "b", id: 1, assigned: 1, client: client, logger: p.logger}
	p.sets = []*scaleSet{a, b}
	p.reconcile(context.Background())
	if boots != 0 {
		t.Fatalf("booted %d runners with unresolved active ownership", boots)
	}
	if _, err := os.Stat(filepath.Join(p.cfg.jitDir, "runner-1")); err != nil {
		t.Fatal("unknown active slot removed")
	}
}

func TestOwnerRecordPrecedesRegistrationAndSurvivesFailedBoot(t *testing.T) {
	p := testPool(t)
	slot := "runner-1"
	os.Mkdir(filepath.Join(p.cfg.jitDir, slot), 0700)
	set := &scaleSet{org: "example", id: 1, logger: p.logger}
	p.sets = []*scaleSet{set}
	called := false
	set.client = testRunnerClient{generate: func(s *scaleset.RunnerScaleSetJitRunnerSetting) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
		called = true
		r, err := p.readOwner(slot)
		if err != nil {
			t.Fatal(err)
		}
		if r.Runner != s.Name || r.Org != "example" {
			t.Fatal("registration preceded correct owner record")
		}
		return nil, errors.New("registration unavailable")
	}}
	if err := p.boot(context.Background(), set, slot); err == nil || !called {
		t.Fatal("expected registration failure")
	}
	restarted := testPool(t)
	restarted.cfg.jitDir = p.cfg.jitDir
	restarted.sets = []*scaleSet{set}
	if err := restarted.restoreOwner(context.Background(), slot, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := restarted.booting[slot]; !ok {
		t.Fatal("pending boot reservation lost across restart")
	}
	info, err := os.Stat(filepath.Join(p.cfg.jitDir, slot, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("owner record mode=%v", info.Mode())
	}
}

func TestMismatchedOwnerDoesNotAdoptActiveRunner(t *testing.T) {
	p := testPool(t)
	slot := "runner-1"
	testIdentity(t, p, slot, "runner-1-12345678")
	r := ownerRecord{Version: 1, Org: "example", ScaleSetID: 1, Runner: "runner-1-87654321", StartedAt: time.Now(), MemoryGiB: 10}
	if err := p.writeOwner(slot, r); err != nil {
		t.Fatal(err)
	}
	p.sets = []*scaleSet{{org: "example", id: 1}}
	if err := p.restoreOwner(context.Background(), slot, true); err == nil {
		t.Fatal("adopted mismatched identity")
	}
	if len(p.owners) != 0 {
		t.Fatal("mismatched active runner was counted as owned")
	}
	if _, err := os.Stat(filepath.Join(p.cfg.jitDir, slot, "jitconfig")); err != nil {
		t.Fatal("active credentials removed")
	}
}

func TestRunnerIdentityErrorsDoNotExposeCredentials(t *testing.T) {
	p := testPool(t)
	slot := "runner-1"
	testIdentity(t, p, slot, "runner-1-12345678")
	ref, err := readRunnerIdentity(filepath.Join(p.cfg.jitDir, slot, "jitconfig"))
	if err != nil {
		t.Fatal(err)
	}
	if ref.AgentName != "runner-1-12345678" {
		t.Fatal("wrong identity")
	}
	file := filepath.Join(p.cfg.jitDir, slot, "jitconfig")
	os.Chmod(file, 0600)
	os.WriteFile(file, []byte("secret-marker-not-base64"), 0600)
	_, err = readRunnerIdentity(file)
	if err == nil || strings.Contains(err.Error(), "secret-marker") {
		t.Fatalf("unsafe decode error: %v", err)
	}
}

func TestRunnerCleanupIsIdempotent(t *testing.T) {
	p := testPool(t)
	slot := "runner-1"
	testIdentity(t, p, slot, "runner-1-12345678")
	removed := 0
	client := testRunnerClient{lookup: func(string) (*scaleset.RunnerReference, error) {
		if removed > 0 {
			return nil, nil
		}
		return &scaleset.RunnerReference{ID: 7}, nil
	}, remove: func(int64) error { removed++; return nil }}
	set := &scaleSet{org: "example", id: 1, client: client}
	r := ownerRecord{Version: 1, Org: "example", ScaleSetID: 1, Runner: "runner-1-12345678", StartedAt: time.Now(), MemoryGiB: 10}
	p.writeOwner(slot, r)
	p.owners[slot] = owner{set: set, runner: r.Runner, record: r}
	if !p.forget(context.Background(), slot) || !p.forget(context.Background(), slot) {
		t.Fatal("cleanup failed")
	}
	if removed != 1 {
		t.Fatalf("runner removed %d times", removed)
	}
	if _, err := os.Stat(filepath.Join(p.cfg.jitDir, slot, "owner.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owner record retained")
	}
}

func TestFailedCleanupRetainsReservation(t *testing.T) {
	p := testPool(t)
	slot := "runner-1"
	testIdentity(t, p, slot, "runner-1-12345678")
	set := &scaleSet{org: "example", client: testRunnerClient{lookup: func(string) (*scaleset.RunnerReference, error) { return nil, errors.New("API unavailable") }}}
	r := ownerRecord{Version: 1, Org: "example", ScaleSetID: 1, Runner: "runner-1-12345678", StartedAt: time.Now(), MemoryGiB: 10}
	p.writeOwner(slot, r)
	p.owners[slot] = owner{set: set, runner: r.Runner, record: r}
	if p.forget(context.Background(), slot) {
		t.Fatal("freed a slot before cleanup succeeded")
	}
	if _, err := os.Stat(filepath.Join(p.cfg.jitDir, slot, "owner.json")); err != nil {
		t.Fatal("lost retry state")
	}
}

func fakeQMP(t *testing.T, path string, handle func(string, map[string]json.RawMessage) any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(2 * time.Second))
				enc := json.NewEncoder(conn)
				dec := json.NewDecoder(conn)
				enc.Encode(map[string]any{"QMP": map[string]any{"version": map[string]any{}}})
				for {
					var req struct {
						Execute string                     `json:"execute"`
						Args    map[string]json.RawMessage `json:"arguments"`
						ID      int                        `json:"id"`
					}
					if dec.Decode(&req) != nil {
						return
					}
					result := handle(req.Execute, req.Args)
					// Events can arrive between a command and its reply.
					if enc.Encode(map[string]any{"event": "BALLOON_CHANGE"}) != nil {
						return
					}
					if enc.Encode(map[string]any{"id": req.ID, "return": result}) != nil {
						return
					}
				}
			}()
		}
	}()
}

func shortQMPPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "qmp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "qmp.sock")
}

func TestQMPSamplesWithoutResettingPolling(t *testing.T) {
	path := shortQMPPath(t)
	var resets atomic.Int64
	fakeQMP(t, path, func(execute string, args map[string]json.RawMessage) any {
		switch execute {
		case "query-memory-size-summary":
			return map[string]int64{"base-memory": 10 * gib}
		case "query-balloon":
			return map[string]int64{"actual": 8 * gib}
		case "qom-set":
			resets.Add(1)
		case "qom-get":
			var property string
			json.Unmarshal(args["property"], &property)
			if property == "guest-stats-polling-interval" {
				return 5
			}
			return map[string]any{"last-update": time.Now().Unix(), "stats": map[string]int64{"stat-available-memory": 5 * gib, "stat-oom-kills": 0, "stat-disk-caches": 4 * gib}}
		}
		return map[string]any{}
	})
	q, err := openQMP(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	sample, err := q.memory()
	if err != nil {
		t.Fatal(err)
	}
	if !sample.fresh(time.Now()) || sample.ActualBytes != 8*gib || sample.CeilingBytes != 10*gib {
		t.Fatalf("invalid sample: %+v", sample)
	}
	if resets.Load() != 0 {
		t.Fatal("reset polling despite an existing interval")
	}
}

func TestQMPStallHasDeadline(t *testing.T) {
	path := shortQMPPath(t)
	release := make(chan struct{})
	defer close(release)
	fakeQMP(t, path, func(string, map[string]json.RawMessage) any { <-release; return map[string]any{} })
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err := openQMP(ctx, path)
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("expected QMP deadline, got %v", err)
	}
}

func TestLegacyAdoptionRequiresUniqueOrganization(t *testing.T) {
	for _, kind := range []string{"unique", "ambiguous", "mismatched", "unavailable"} {
		t.Run(kind, func(t *testing.T) {
			p := testPool(t)
			slot := "runner-1"
			name := "runner-1-12345678"
			testIdentity(t, p, slot, name)
			first := testRunnerClient{lookup: func(string) (*scaleset.RunnerReference, error) {
				return &scaleset.RunnerReference{ID: 7, Name: name, RunnerScaleSetID: 1}, nil
			}}
			second := testRunnerClient{lookup: func(string) (*scaleset.RunnerReference, error) {
				switch kind {
				case "ambiguous":
					return &scaleset.RunnerReference{ID: 7, Name: name, RunnerScaleSetID: 1}, nil
				case "unavailable":
					return nil, errors.New("unavailable")
				}
				return nil, nil
			}}
			if kind == "mismatched" {
				first.lookup = func(string) (*scaleset.RunnerReference, error) {
					return &scaleset.RunnerReference{ID: 8, Name: name, RunnerScaleSetID: 1}, nil
				}
			}
			p.sets = []*scaleSet{{org: "a", id: 1, client: first}, {org: "b", id: 1, client: second}}
			if kind == "unique" {
				path := shortQMPPath(t)
				p.cfg.stateDir = filepath.Dir(filepath.Dir(path))
				// Use the controller's actual socket layout, with a short root path.
				p.cfg.stateDir = filepath.Dir(path)
				path = p.socketPath(slot)
				fakeQMP(t, path, func(execute string, _ map[string]json.RawMessage) any {
					if execute == "query-memory-size-summary" {
						return map[string]int64{"base-memory": 10 * gib}
					}
					return map[string]any{}
				})
				p.cfg.vmMemGiB = 8
			}
			err := p.restoreOwner(context.Background(), slot, true)
			if kind == "unique" {
				if err != nil {
					t.Fatal(err)
				}
				if p.owners[slot].set.org != "a" || p.owners[slot].record.MemoryGiB != 10 {
					t.Fatal("adoption lost organization or original boot ceiling")
				}
				if _, err := p.readOwner(slot); err != nil {
					t.Fatal("adoption was not persisted")
				}
			} else if err == nil || len(p.owners) != 0 {
				t.Fatal("adopted an unproven runner identity")
			}
		})
	}
}

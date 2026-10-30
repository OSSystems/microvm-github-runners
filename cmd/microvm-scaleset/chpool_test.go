package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

func testManifest() guestManifest {
	return guestManifest{
		CloudHypervisor: "/nix/store/ch/bin/cloud-hypervisor",
		Virtiofsd:       "/nix/store/vfs/bin/virtiofsd",
		Kernel:          "/nix/store/kernel/bzImage",
		Initrd:          "/nix/store/initrd/initrd",
		Cmdline:         "console=ttyS0 init=/nix/store/system/init",
		Shares:          []guestShare{{Tag: "ro-store", Source: "/nix/store"}},
		BootMemMiB:      2048,
		HotplugMemMiB:   1048576,
	}
}

type fakeVMs struct {
	mu       sync.Mutex
	booted   [][]string
	live     map[string]bool
	stopped  []string
	leftover []string
	mem      map[string]int64
	resized  map[string]int64
	bootErr  error
	// called runs first in every call that may be slow for real.
	called func(string)
	// onMemory and onResize run inside memory and resize calls.
	onMemory func(dir string)
	onResize func(dir string) error
}

func (f *fakeVMs) call(what string) {
	if f.called != nil {
		f.called(what)
	}
}

func (f *fakeVMs) boot(_ context.Context, argv []string) error {
	f.call("boot")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bootErr != nil {
		return f.bootErr
	}
	f.booted = append(f.booted, argv)
	f.live[strings.TrimPrefix(argv[1], "--unit=")] = true
	return nil
}

func (f *fakeVMs) active(_ context.Context, unit string) (bool, error) {
	f.call("active")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live[unit], nil
}

func (f *fakeVMs) stop(_ context.Context, unit string) error {
	f.call("stop")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, unit)
	delete(f.live, unit)
	return nil
}

func (f *fakeVMs) units(context.Context, string) ([]string, error) { return f.leftover, nil }

func (f *fakeVMs) memory(_ context.Context, dir string) (int64, error) {
	f.call("memory")
	if f.onMemory != nil {
		f.onMemory(dir)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.mem[dir]; ok {
		return m, nil
	}
	return 2 * gib, nil
}

func (f *fakeVMs) resize(_ context.Context, dir string, desired int64) error {
	f.call("resize")
	if f.onResize != nil {
		if err := f.onResize(dir); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resized[dir] = desired
	f.mem[dir] = desired
	return nil
}

type fakeHost struct {
	h      hostState
	called func(string)
}

func (f *fakeHost) state(string) (hostState, error) {
	if f.called != nil {
		f.called("host probe")
	}
	return f.h, nil
}

type fakeNet struct {
	taps    map[string]int
	deleted []string
}

func (f *fakeNet) createTap(name string, uid int, bridge string) error {
	if bridge != "microbr" {
		return errors.New("wrong bridge")
	}
	f.taps[name] = uid
	return nil
}

func (f *fakeNet) deleteTap(name string) error {
	delete(f.taps, name)
	f.deleted = append(f.deleted, name)
	return nil
}

type chTestEnv struct {
	pool    *chPool
	vms     *fakeVMs
	host    *fakeHost
	net     *fakeNet
	set     *scaleSet
	removed []int64
	known   map[string]int
	// generate runs inside GenerateJitRunnerConfig.
	generate func()
}

func newCHTestEnv(t *testing.T) *chTestEnv {
	t.Helper()
	// Short: the control socket path must fit in sun_path.
	work, err := os.MkdirTemp("", "ch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(work) })
	c := chTestConfig()
	c.name, c.ch.workDir, c.ch.bridge, c.ch.nixDaemonPort, c.ch.controlPort, c.ch.vmUser = "ci", work, "microbr", 5000, 5001, "microvm"
	e := &chTestEnv{
		vms:   &fakeVMs{live: map[string]bool{}, mem: map[string]int64{}, resized: map[string]int64{}},
		host:  &fakeHost{h: hostState{load1: 1, cpus: 16, memTotal: 64 * gib, memAvailable: 60 * gib, diskFree: 900 * gib, diskTotal: 1000 * gib}},
		net:   &fakeNet{taps: map[string]int{}},
		known: map[string]int{},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.set = &scaleSet{org: "example", id: 7, logger: logger}
	// systemd, cloud-hypervisor and GitHub calls must not hold the pool lock.
	unlocked := func(what string) {
		if !e.pool.mu.TryLock() {
			t.Errorf("%s called with the pool lock held", what)
			return
		}
		e.pool.mu.Unlock()
	}
	e.vms.called = unlocked
	e.host.called = unlocked
	e.set.client = testRunnerClient{
		generate: func(s *scaleset.RunnerScaleSetJitRunnerSetting) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
			unlocked("GenerateJitRunnerConfig")
			if e.generate != nil {
				e.generate()
			}
			e.known[s.Name] = len(e.known) + 100
			return &scaleset.RunnerScaleSetJitRunnerConfig{EncodedJITConfig: "jit-for-" + s.Name}, nil
		},
		lookup: func(name string) (*scaleset.RunnerReference, error) {
			unlocked("GetRunnerByName")
			if id, ok := e.known[name]; ok {
				return &scaleset.RunnerReference{ID: id, Name: name}, nil
			}
			return nil, nil
		},
		remove: func(id int64) error {
			e.removed = append(e.removed, id)
			for name, known := range e.known {
				if int64(known) == id {
					delete(e.known, name)
				}
			}
			return nil
		},
	}
	e.pool = &chPool{
		cfg:      c,
		logger:   logger,
		manifest: testManifest(),
		addrs:    newAddressPool([3]byte{10, 231, 0}),
		account:  account{uid: os.Getuid(), gid: os.Getgid()},
		self:     "/bin/microvm-scaleset",
		cpus:     16,
		vms:      e.vms,
		host:     e.host,
		net:      e.net,
		kick:     make(chan struct{}, 1),
		running:  map[string]*chVM{},
		sets:     []*scaleSet{e.set},
	}
	e.set.backend = e.pool
	return e
}

// desired tells the pool about n jobs, runs one reconcile pass and returns
// the scale set's runners.
func (e *chTestEnv) desired(n int) int {
	e.pool.desiredRunners(context.Background(), e.set, n)
	e.pool.reconcile(context.Background())
	e.pool.mu.Lock()
	defer e.pool.mu.Unlock()
	return e.pool.counts()[e.set]
}

func (e *chTestEnv) only(t *testing.T) *chVM {
	t.Helper()
	if len(e.pool.running) != 1 {
		t.Fatalf("%d VMs running, want 1", len(e.pool.running))
	}
	for _, vm := range e.pool.running {
		return vm
	}
	return nil
}

func TestCHBootServesJITAndTearsDown(t *testing.T) {
	e := newCHTestEnv(t)
	if got := e.desired(1); got != 1 {
		t.Fatalf("runners = %d, want 1", got)
	}
	var vm *chVM
	for _, v := range e.pool.running {
		vm = v
	}
	if !strings.HasPrefix(vm.runner, "ci-10-") || len(vm.runner) != len("ci-10-")+8 || vm.unit != "ghr-ci-10.service" {
		t.Fatalf("runner %s unit %s", vm.runner, vm.unit)
	}
	disk, err := os.Stat(filepath.Join(vm.dir, "disk.img"))
	if err != nil || disk.Size() != 1<<40 {
		t.Fatalf("disk = %v, %v", disk, err)
	}
	if st := disk.Sys().(*syscall.Stat_t); st.Blocks > 8 {
		t.Fatalf("disk is not sparse: %d blocks", st.Blocks)
	}
	if uid, ok := e.net.taps["ghrunner10"]; !ok || uid != os.Getuid() {
		t.Fatalf("taps = %v", e.net.taps)
	}
	if _, err := os.Stat(vm.specPath); err != nil {
		t.Fatal(err)
	}
	if len(e.vms.booted) != 1 || e.vms.booted[0][0] != "systemd-run" {
		t.Fatalf("booted = %q", e.vms.booted)
	}
	reply := controlCall(t, vsockPortSocket(vm.dir, 5001), `{"op":"jitconfig"}`)
	if reply["jitconfig"] != "jit-for-"+vm.runner {
		t.Fatalf("jitconfig reply = %v", reply)
	}

	// The job ran; the guest powered off and its unit ended.
	e.vms.live[vm.unit] = false
	if got := e.desired(0); got != 0 {
		t.Fatalf("runners after power-off = %d", got)
	}
	if len(e.pool.running) != 0 || e.pool.addrs.used[10] {
		t.Fatal("VM or its address was kept")
	}
	if _, err := os.Stat(vm.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("VM dir kept: %v", err)
	}
	if _, err := os.Stat(vm.specPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spec kept: %v", err)
	}
	if !slices.Contains(e.net.deleted, "ghrunner10") || len(e.removed) != 1 {
		t.Fatalf("tap deleted %v, runners removed %v", e.net.deleted, e.removed)
	}
}

func TestCHAdmissionCountsPendingBoots(t *testing.T) {
	e := newCHTestEnv(t)
	// Reserve 6.4 GiB; 13 GiB available leaves room for three 2 GiB boots
	// while none of them has reported yet.
	e.host.h.memAvailable = 13 * gib
	if got := e.desired(10); got != 3 {
		t.Fatalf("runners = %d, want 3 admitted", got)
	}
	if e.pool.busyReason == "" || e.set.waiting != 7 {
		t.Fatalf("busy %q, waiting %d", e.pool.busyReason, e.set.waiting)
	}
	// Once a VM reported, its boot memory shows in host MemAvailable instead.
	for _, vm := range e.pool.running {
		vm.recordStats(guestStats{MemAvailableKiB: 1 << 20, MemTotalKiB: 2 << 20, ReceivedAt: time.Now()})
		break
	}
	if got := e.desired(10); got != 4 {
		t.Fatalf("runners = %d, want one more", got)
	}
	e.host.h.load1 = 16
	if got := e.desired(10); got != 4 {
		t.Fatalf("booted above the load limit: %d", got)
	}
}

func TestCHFailedBootReleasesEverything(t *testing.T) {
	e := newCHTestEnv(t)
	e.vms.bootErr = errors.New("systemd-run failed")
	if got := e.desired(1); got != 0 {
		t.Fatalf("runners = %d", got)
	}
	if len(e.pool.running) != 0 || len(e.pool.addrs.used) != 0 || len(e.net.taps) != 0 || len(e.known) != 0 {
		t.Fatalf("leaked: running %d, addresses %v, taps %v, runners %v", len(e.pool.running), e.pool.addrs.used, e.net.taps, e.known)
	}
	entries, _ := os.ReadDir(e.pool.cfg.ch.workDir)
	if len(entries) != 0 {
		t.Fatalf("work dir not empty: %v", entries)
	}
}

func TestCHStartRemovesLeftovers(t *testing.T) {
	e := newCHTestEnv(t)
	work := e.pool.cfg.ch.workDir
	for _, p := range []string{"ci-11-0badc0de", "ci-11-0badc0de.json", "ci-110-deadbeef", "other-12-0badc0de", "ci-notavm"} {
		if strings.HasSuffix(p, ".json") {
			os.WriteFile(filepath.Join(work, p), nil, 0600)
		} else {
			os.Mkdir(filepath.Join(work, p), 0700)
		}
	}
	e.vms.leftover = []string{"ghr-ci-11.service", "ghr-ci-other-3.service"}
	e.vms.live["ghr-ci-11.service"] = true
	e.net.taps["ghrunner11"] = 0
	// Another instance's tap; this controller has nothing at .50.
	e.net.taps["ghrunner50"] = 0
	e.known["ci-11-0badc0de"] = 5
	e.pool.start(context.Background(), []*scaleSet{e.set})
	if !slices.Equal(e.vms.stopped, []string{"ghr-ci-11.service"}) {
		t.Fatalf("stopped %v", e.vms.stopped)
	}
	if len(e.net.taps) != 1 || e.net.taps["ghrunner50"] != 0 || !slices.Contains(e.net.deleted, "ghrunner11") || !slices.Equal(e.removed, []int64{5}) {
		t.Fatalf("taps %v, removed runners %v", e.net.taps, e.removed)
	}
	var left []string
	entries, _ := os.ReadDir(work)
	for _, entry := range entries {
		left = append(left, entry.Name())
	}
	if !slices.Equal(left, []string{"ci-notavm", "other-12-0badc0de"}) {
		t.Fatalf("left in work dir: %v", left)
	}
}

func TestCHMemoryControl(t *testing.T) {
	e := newCHTestEnv(t)
	e.host.h.memAvailable = 20 * gib
	if got := e.desired(2); got != 2 {
		t.Fatal(got)
	}
	var vms []*chVM
	for _, vm := range e.pool.running {
		vms = append(vms, vm)
	}
	now := time.Now()
	pressured := guestStats{MemAvailableKiB: 512 << 10, MemTotalKiB: 2 << 20, PSISomeAvg10: 20, ReceivedAt: now.Add(-time.Second)}
	for _, vm := range vms {
		vm.recordStats(pressured)
	}
	// Reserve 6.4 GiB of 20 GiB available leaves 13.6 GiB: both may grow.
	e.pool.controlMemory(context.Background(), now)
	for _, vm := range vms {
		if e.vms.resized[vm.dir] != 3*gib {
			t.Fatalf("%s resized to %d", vm.runner, e.vms.resized[vm.dir])
		}
		if vm.pending() != gib {
			t.Fatalf("plug not reserved: %d", vm.pending())
		}
	}
	// Without new stats nothing changes again.
	e.pool.controlMemory(context.Background(), now.Add(time.Second))
	if len(e.vms.resized) != 2 || e.vms.resized[vms[0].dir] != 3*gib {
		t.Fatalf("resized again before the guest reported: %v", e.vms.resized)
	}
	// The host fills up: the next plug is denied and admission closes.
	e.host.h.memAvailable = 7 * gib
	pressured.ReceivedAt = now.Add(2 * time.Second)
	pressured.MemTotalKiB = 3 << 20
	vms[0].recordStats(pressured)
	e.pool.controlMemory(context.Background(), now.Add(3*time.Second))
	if e.vms.resized[vms[0].dir] != 3*gib || !vms[0].denied {
		t.Fatalf("plug granted on a full host: %v", e.vms.resized)
	}
	e.host.h.memAvailable = 40 * gib
	if got := e.desired(3); got != 2 {
		t.Fatalf("admitted a VM while guests were denied memory: %d", got)
	}
	// Stale stats freeze changes even with room.
	e.pool.controlMemory(context.Background(), now.Add(20*time.Second))
	if e.vms.resized[vms[0].dir] != 3*gib {
		t.Fatal("acted on stale stats")
	}
}

func TestCHWatchdogStopsStuckVMs(t *testing.T) {
	tests := []struct {
		name  string
		stuck func(*chVM)
		stop  bool
	}{
		{"booting within the JIT timeout", func(vm *chVM) { vm.startedAt = time.Now().Add(-jitTimeout + time.Minute) }, false},
		{"never fetched its JIT config", func(vm *chVM) { vm.startedAt = time.Now().Add(-jitTimeout - time.Second) }, true},
		{"reporting", func(vm *chVM) {
			vm.jitState = jitServed
			vm.startedAt = time.Now().Add(-time.Hour)
			vm.mem.stats.ReceivedAt = time.Now().Add(-time.Second)
		}, false},
		{"fetched its JIT config, not reporting yet", func(vm *chVM) {
			vm.jitState = jitServed
			vm.startedAt = time.Now().Add(-jitTimeout + time.Minute)
		}, false},
		{"fetched its JIT config, never reported", func(vm *chVM) {
			vm.jitState = jitServed
			vm.startedAt = time.Now().Add(-jitTimeout - time.Second)
		}, true},
		{"stopped reporting", func(vm *chVM) {
			vm.jitState = jitServed
			vm.mem.stats.ReceivedAt = time.Now().Add(-statsTimeout - time.Second)
		}, true},
		{"just finished its job", func(vm *chVM) {
			vm.jitState = jitServed
			vm.completedAt = time.Now().Add(-time.Second)
		}, false},
		{"up long after its job", func(vm *chVM) {
			vm.jitState = jitServed
			vm.completedAt = time.Now().Add(-jobEndGrace - time.Second)
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newCHTestEnv(t)
			if got := e.desired(1); got != 1 {
				t.Fatal(got)
			}
			vm := e.only(t)
			tt.stuck(vm)
			got := e.desired(0)
			if tt.stop != (got == 0) || tt.stop != slices.Contains(e.vms.stopped[1:], vm.unit) {
				t.Fatalf("runners %d, stopped %v; want stop %v", got, e.vms.stopped, tt.stop)
			}
			if tt.stop && (e.pool.addrs.used[vm.addr.octet] || len(e.known) != 0) {
				t.Fatal("stuck VM's address or runner kept")
			}
		})
	}
}

func TestCHJobCompletedEndsTheVM(t *testing.T) {
	e := newCHTestEnv(t)
	e.desired(1)
	vm := e.only(t)
	ctx := context.Background()
	e.set.HandleJobCompleted(ctx, &scaleset.JobCompleted{RunnerName: "someone-else"})
	e.set.HandleJobCompleted(ctx, &scaleset.JobCompleted{RunnerName: vm.runner})
	vm.mu.Lock()
	completed := vm.completedAt
	vm.jitState = jitServed
	vm.mu.Unlock()
	if completed.IsZero() || time.Since(completed) > time.Minute {
		t.Fatalf("completion not recorded: %v", completed)
	}
	select {
	case <-e.pool.kick:
	default:
		t.Fatal("job completion did not ask for a reconcile")
	}
	vm.markCompleted(time.Now())
	vm.mu.Lock()
	vm.completedAt = time.Now().Add(-jobEndGrace - time.Second)
	vm.mu.Unlock()
	if got := e.desired(0); got != 0 {
		t.Fatalf("VM kept after its job: %d", got)
	}
}

// A listener's callback returns at once even while a boot waits on GitHub.
func TestCHListenerDoesNotWaitForBoots(t *testing.T) {
	e := newCHTestEnv(t)
	release := make(chan struct{})
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	entered := make(chan struct{})
	var once sync.Once
	e.generate = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	e.set.assigned = 1
	done := make(chan struct{})
	go func() {
		e.pool.reconcile(context.Background())
		close(done)
	}()
	<-entered
	returned := make(chan int)
	go func() { returned <- e.pool.desiredRunners(context.Background(), e.set, 2) }()
	select {
	case n := <-returned:
		if n != 1 {
			t.Fatalf("runners = %d, want the booting VM counted", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("desiredRunners waited for a boot")
	}
	released.Do(func() { close(release) })
	<-done
}

// admissionNow is what reconcile would decide at this moment.
func (e *chTestEnv) admissionNow() string {
	h, err := e.host.state("")
	e.pool.mu.Lock()
	defer e.pool.mu.Unlock()
	return e.pool.admission(time.Now(), h, err)
}

// A plug counts as promised from the moment it is decided, not when the
// resize request returns, and is taken back if the request fails.
func TestCHPlugIsPendingDuringItsResize(t *testing.T) {
	e := newCHTestEnv(t)
	e.desired(1)
	vm := e.only(t)
	now := time.Now()
	vm.recordStats(guestStats{MemAvailableKiB: 512 << 10, MemTotalKiB: 2 << 20, PSISomeAvg10: 20, ReceivedAt: now.Add(-time.Second)})
	// Reserve 6.4 GiB; 2.5 GiB spare fits the 1 GiB plug or a 2 GiB boot,
	// not both.
	e.host.h.memAvailable = 64*gib/10 + 5*gib/2
	if reason := e.admissionNow(); reason != "" {
		t.Fatalf("admission closed before the plug: %s", reason)
	}
	during := ""
	e.vms.onResize = func(string) error {
		e.host.called = nil
		during = e.admissionNow()
		return errors.New("VMM busy")
	}
	e.pool.controlMemory(context.Background(), now)
	if !strings.Contains(during, "pending") {
		t.Fatalf("admission during the plug = %q, want it closed by the pending plug", during)
	}
	if vm.pending() != 0 {
		t.Fatalf("failed plug still pending: %d", vm.pending())
	}
	if reason := e.admissionNow(); reason != "" {
		t.Fatalf("admission after the failed plug: %s", reason)
	}
}

// A boot admitted while the memory loop is busy counts against the plugs it
// decides afterwards.
func TestCHPlugSeesBootsAdmittedDuringTheTick(t *testing.T) {
	e := newCHTestEnv(t)
	e.desired(1)
	vm := e.only(t)
	now := time.Now()
	vm.recordStats(guestStats{MemAvailableKiB: 512 << 10, MemTotalKiB: 2 << 20, PSISomeAvg10: 20, ReceivedAt: now.Add(-time.Second)})
	e.host.h.memAvailable = 64*gib/10 + 5*gib/2
	e.vms.onMemory = func(string) {
		e.vms.onMemory = nil
		e.pool.mu.Lock()
		e.pool.reserve(e.set, time.Now())
		e.pool.mu.Unlock()
	}
	e.pool.controlMemory(context.Background(), now)
	if len(e.vms.resized) != 0 || !vm.denied {
		t.Fatalf("plug granted against headroom a new boot holds: %v", e.vms.resized)
	}
}

func TestLeftovers(t *testing.T) {
	l := leftovers("ci", []string{"ghr-ci-11.service", "ghr-ci-other-3.service", "ghr-cix-12.service"},
		[]string{"ci-11-0badc0de", "ci-11-0badc0de.json", "ci-110-deadbeef", "ci-300-deadbeef", "other-12-0badc0de", "ci-notavm"})
	if !slices.Equal(l.units, []string{"ghr-ci-11.service"}) ||
		!slices.Equal(l.paths, []string{"ci-11-0badc0de", "ci-11-0badc0de.json", "ci-110-deadbeef", "ci-300-deadbeef"}) ||
		!slices.Equal(l.runners, []string{"ci-11-0badc0de", "ci-110-deadbeef", "ci-300-deadbeef"}) ||
		!slices.Equal(l.taps, []string{"ghrunner11", "ghrunner110"}) {
		t.Fatalf("leftovers = %+v", l)
	}
}

// Host memory and pending memory are read together: a boot that turned
// from pending into used memory during the tick is not counted as spare.
func TestCHPlugUsesAFreshHostProbe(t *testing.T) {
	e := newCHTestEnv(t)
	e.desired(2)
	var a, b *chVM
	for _, vm := range e.pool.running {
		if a == nil {
			a = vm
		} else {
			b = vm
		}
	}
	now := time.Now()
	a.recordStats(guestStats{MemAvailableKiB: 512 << 10, MemTotalKiB: 2 << 20, PSISomeAvg10: 20, ReceivedAt: now.Add(-time.Second)})
	// Reserve 6.4 GiB; 2.5 GiB available above it, 2 GiB of it promised to b.
	e.host.h.memAvailable = 64*gib/10 + 5*gib/2
	e.vms.onMemory = func(dir string) {
		if dir != a.dir {
			return
		}
		// b boots meanwhile: its memory moves from pending to used.
		b.recordStats(guestStats{MemAvailableKiB: 2 << 20, MemTotalKiB: 2 << 20, ReceivedAt: time.Now()})
		e.host.h.memAvailable = 64*gib/10 + gib/2
	}
	e.pool.controlMemory(context.Background(), now)
	if _, plugged := e.vms.resized[a.dir]; plugged || !a.denied {
		t.Fatalf("plug granted from a stale host probe: %v", e.vms.resized)
	}
}

// A failed unplug changes nothing: the calm guest is unplugged at the next
// tick, not after another calm period.
func TestCHFailedUnplugRetriesAtOnce(t *testing.T) {
	e := newCHTestEnv(t)
	e.desired(1)
	vm := e.only(t)
	now := time.Now()
	e.vms.mem[vm.dir] = 4 * gib
	calm := guestStats{MemAvailableKiB: 7 << 19, MemTotalKiB: 4 << 20, ReceivedAt: now.Add(-31 * time.Second)}
	vm.recordStats(calm)
	calm.ReceivedAt = now.Add(-time.Second)
	vm.recordStats(calm)
	e.vms.onResize = func(string) error { return errors.New("VMM busy") }
	e.pool.controlMemory(context.Background(), now)
	e.vms.onResize = nil
	e.pool.controlMemory(context.Background(), now.Add(time.Second))
	if e.vms.resized[vm.dir] != 3*gib {
		t.Fatalf("resized = %v, want the unplug retried at once", e.vms.resized)
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/actions/scaleset"
)

const (
	// diskBytes is the sparse disk every VM gets; only what the job writes
	// takes space.
	diskBytes = int64(1) << 40
	// jitTimeout is how long a guest may take to fetch its JIT config.
	jitTimeout = 3 * time.Minute
	// statsTimeout is how long a guest that reported once may go silent.
	statsTimeout = time.Minute
	// jobEndGrace is how long a VM may stay up after GitHub reported its job
	// completed; the guest normally powers off well within it.
	jobEndGrace = 30 * time.Second
)

// vmHost boots, resizes, inspects and stops cloud-hypervisor VMs; tests
// replace it.
type vmHost interface {
	// boot starts a VM's unit from its argv.
	boot(ctx context.Context, argv []string) error
	// active reports whether a unit still runs; errors count as running.
	active(ctx context.Context, unit string) (bool, error)
	// stop stops a unit and forgets it; a unit that is gone is no error.
	stop(ctx context.Context, unit string) error
	// units lists the loaded units matching a glob.
	units(ctx context.Context, glob string) ([]string, error)
	// memory returns a VM's boot plus plugged memory, in bytes.
	memory(ctx context.Context, dir string) (int64, error)
	// resize asks a VM for desired bytes of memory.
	resize(ctx context.Context, dir string, desired int64) error
}

// hostProbe reads the host's load, memory and work-dir disk; tests replace it.
type hostProbe interface {
	state(workDir string) (hostState, error)
}

const (
	jitWaiting = iota
	jitSending
	jitServed
)

// chVM is one cloud-hypervisor VM running one job.
type chVM struct {
	set       *scaleSet
	runner    string
	unit      string
	dir       string
	specPath  string
	addr      vmAddress
	startedAt time.Time
	ln        net.Listener

	mu       sync.Mutex
	jit      string
	jitState int
	mem      vmMemory
	denied   bool
	// completedAt is when GitHub reported the job done.
	completedAt time.Time
}

func (vm *chVM) takeJITConfig() (string, func(bool), error) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	switch vm.jitState {
	case jitSending:
		return "", nil, errors.New("jitconfig is being served")
	case jitServed:
		return "", nil, errors.New("jitconfig already served for this boot")
	}
	vm.jitState = jitSending
	return vm.jit, func(delivered bool) {
		vm.mu.Lock()
		defer vm.mu.Unlock()
		if delivered {
			vm.jitState = jitServed
			vm.jit = ""
		} else {
			vm.jitState = jitWaiting
		}
	}, nil
}

func (vm *chVM) recordStats(s guestStats) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.mem.observe(s)
}

// pending is memory this VM was promised but has not been seen to take: its
// boot memory until it first reports, then a plug its guest does not show
// yet.
func (vm *chVM) pending() int64 {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.mem.stats.ReceivedAt.IsZero() {
		return vm.mem.bootBytes
	}
	return vm.mem.pendingPlug
}

// memorySnapshot updates the VM's actual size and returns its memory state.
func (vm *chVM) memorySnapshot(actual int64) vmMemory {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.mem.actualBytes = actual
	return vm.mem
}

// grantResize records a decided resize before it is requested and returns
// how to take it back if the request fails.
func (vm *chVM) grantResize(actual, target int64, at time.Time) (undo func()) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	previous, wasDenied := vm.mem.grant(actual, target, at), vm.denied
	vm.denied = false
	return func() {
		vm.mu.Lock()
		defer vm.mu.Unlock()
		// Everything but what was observed since goes back.
		stats, actual := vm.mem.stats, vm.mem.actualBytes
		vm.mem, vm.denied = previous, wasDenied
		vm.mem.stats, vm.mem.actualBytes = stats, actual
	}
}

// resizeDone notes when a resize request finished: only stats newer than
// that show it.
func (vm *chVM) resizeDone(at time.Time) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.mem.resizedAt = at
}

// markDenied notes a denied plug and reports whether it is the first in a
// row.
func (vm *chVM) markDenied() bool {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	first := !vm.denied
	vm.denied = true
	return first
}

func (vm *chVM) markCompleted(at time.Time) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.completedAt.IsZero() {
		vm.completedAt = at
	}
}

// expired reports why a running VM should be stopped, or "": it never
// fetched its JIT config, its guest never reported or stopped reporting, or
// it outlived its job.
func (vm *chVM) expired(now time.Time) string {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	switch {
	case vm.jitState != jitServed && now.Sub(vm.startedAt) > jitTimeout:
		return "runner did not fetch its JIT config"
	case vm.mem.stats.ReceivedAt.IsZero() && now.Sub(vm.startedAt) > jitTimeout:
		return "guest never reported"
	case !vm.mem.stats.ReceivedAt.IsZero() && now.Sub(vm.mem.stats.ReceivedAt) > statsTimeout:
		return "guest stopped reporting"
	case !vm.completedAt.IsZero() && now.Sub(vm.completedAt) > jobEndGrace:
		return "VM still up after its job completed"
	}
	return ""
}

// chPool runs one throwaway cloud-hypervisor VM per job, as many as the host
// admits: no pool size, every VM may use all CPUs and grow its memory while
// the host keeps its reserve.
type chPool struct {
	cfg      config
	logger   *slog.Logger
	manifest guestManifest
	account  account
	self     string
	cpus     int
	vms      vmHost
	host     hostProbe
	net      netOps
	// kick asks the reconcile loop for a pass.
	kick chan struct{}

	// mu guards the fields below and the scale sets' counters. It is never
	// held across systemd, GitHub, cloud-hypervisor or host probes. Every
	// promise of host memory, a boot or a plug, is decided and recorded under
	// it, so two promises never count the same headroom.
	mu         sync.Mutex
	sets       []*scaleSet
	addrs      *addressPool
	running    map[string]*chVM
	forgets    []string
	busyReason string
	next       int
	// deniedAt is when a plug was last denied.
	deniedAt time.Time
}

func newCHPool(c config, logger *slog.Logger) (*chPool, error) {
	raw, err := readBounded(c.ch.guestManifest, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("reading guest manifest: %w", err)
	}
	m, err := parseGuestManifest(raw)
	if err != nil {
		return nil, err
	}
	subnet, err := subnetPrefix(c.ch.subnet)
	if err != nil {
		return nil, err
	}
	acct, err := lookupAccount(c.ch.vmUser)
	if err != nil {
		return nil, fmt.Errorf("--vm-user: %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.ch.workDir, 0755); err != nil {
		return nil, err
	}
	return &chPool{
		cfg:      c,
		logger:   logger,
		manifest: m,
		addrs:    newAddressPool(subnet),
		account:  acct,
		self:     self,
		cpus:     runtime.NumCPU(),
		vms:      systemdVMs{},
		host:     procHost{},
		net:      linuxNet{},
		kick:     make(chan struct{}, 1),
		running:  map[string]*chVM{},
	}, nil
}

func (p *chPool) maxRunners() int { return p.addrs.size() }

// start cleans up after a previous controller: a restart ends every VM it
// ran, so leftover units, taps and directories go, and their runners are
// removed from GitHub. Only what carries this controller's --name is
// touched: taps by the addresses of its leftover units and directories.
func (p *chPool) start(ctx context.Context, sets []*scaleSet) {
	p.mu.Lock()
	p.sets = sets
	p.mu.Unlock()
	units, err := p.vms.units(ctx, "ghr-"+p.cfg.name+"-*")
	if err != nil {
		p.logger.Warn("listing leftover VM units", "error", err)
	}
	var entries []string
	dir, _ := os.ReadDir(p.cfg.ch.workDir)
	for _, e := range dir {
		entries = append(entries, e.Name())
	}
	l := leftovers(p.cfg.name, units, entries)
	for _, unit := range l.units {
		if err := p.vms.stop(ctx, unit); err != nil {
			p.logger.Warn("stopping leftover VM", "unit", unit, "error", err)
		} else {
			p.logger.Info("stopped leftover VM", "unit", unit)
		}
	}
	for _, path := range l.paths {
		if err := os.RemoveAll(filepath.Join(p.cfg.ch.workDir, path)); err != nil {
			p.logger.Warn("removing leftover VM files", "path", path, "error", err)
		}
	}
	for _, tap := range l.taps {
		if err := p.net.deleteTap(tap); err != nil {
			p.logger.Warn("deleting leftover tap", "tap", tap, "error", err)
		}
	}
	p.mu.Lock()
	p.forgets = append(p.forgets, l.runners...)
	p.mu.Unlock()
	p.retryForgets(ctx)
	p.logger.Info("cloud-hypervisor pool ready", "cpusPerVM", p.cpus, "bootMemMiB", p.manifest.BootMemMiB, "hotplugMemMiB", p.manifest.HotplugMemMiB, "addresses", p.addrs.size())
}

// leftover is what a previous controller with this --name left behind.
type leftover struct {
	units, paths, taps, runners []string
}

// leftovers picks this controller's units, work-dir entries, taps and
// runners out of what systemd and the work dir list. A VM's directory is
// created before its tap and removed after it, so the addresses of leftover
// units and directories name every tap a crash can have left.
func leftovers(name string, units, entries []string) leftover {
	quoted := regexp.QuoteMeta(name)
	unitPattern := regexp.MustCompile(`^ghr-` + quoted + `-([0-9]+)\.service$`)
	entryPattern := regexp.MustCompile(`^` + quoted + `-([0-9]+)-[0-9a-f]{8}(\.json)?$`)
	var l leftover
	octets := map[int]bool{}
	for _, unit := range units {
		if m := unitPattern.FindStringSubmatch(unit); m != nil {
			octet, _ := strconv.Atoi(m[1])
			octets[octet] = true
			l.units = append(l.units, unit)
		}
	}
	for _, entry := range entries {
		m := entryPattern.FindStringSubmatch(entry)
		if m == nil {
			continue
		}
		octet, _ := strconv.Atoi(m[1])
		octets[octet] = true
		l.paths = append(l.paths, entry)
		if m[2] == "" {
			l.runners = append(l.runners, entry)
		}
	}
	for octet := firstOctet; octet <= lastOctet; octet++ {
		if octets[octet] {
			l.taps = append(l.taps, vmAddress{octet: octet}.tap())
		}
	}
	return l
}

// trigger asks the reconcile loop for a pass without waiting for it.
func (p *chPool) trigger() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

func (p *chPool) tick(context.Context) { p.trigger() }

// desiredRunners records the assignment and leaves booting to the reconcile
// loop, so a listener never waits on systemd or GitHub.
func (p *chPool) desiredRunners(_ context.Context, s *scaleSet, assigned int) int {
	p.mu.Lock()
	s.assigned = assigned
	n := p.counts()[s]
	p.mu.Unlock()
	p.trigger()
	return n
}

func (p *chPool) jobCompleted(s *scaleSet, runner string, now time.Time) {
	p.mu.Lock()
	vm, ok := p.running[runner]
	p.mu.Unlock()
	if ok && vm.set == s {
		vm.markCompleted(now)
		p.trigger()
	}
}

// counts is how many VMs each scale set has. The caller holds p.mu.
func (p *chPool) counts() map[*scaleSet]int {
	running := map[*scaleSet]int{}
	for _, vm := range p.running {
		running[vm.set]++
	}
	return running
}

// snapshot lists the VMs. The caller holds p.mu.
func (p *chPool) snapshot() []*chVM {
	vms := make([]*chVM, 0, len(p.running))
	for _, vm := range p.running {
		vms = append(vms, vm)
	}
	return vms
}

// pendingBytes is the memory promised to vms that they have not been seen
// to take.
func pendingBytes(vms []*chVM) int64 {
	var pending int64
	for _, vm := range vms {
		pending += vm.pending()
	}
	return pending
}

func (p *chPool) retryForgets(ctx context.Context) {
	p.mu.Lock()
	names := p.forgets
	p.forgets = nil
	sets := p.sets
	p.mu.Unlock()
	var left []string
	for _, name := range names {
		if err := forgetRunner(ctx, sets, name); err != nil {
			p.logger.Warn("runner cleanup deferred", "runner", name, "error", err)
			left = append(left, name)
		}
	}
	p.mu.Lock()
	p.forgets = append(p.forgets, left...)
	p.mu.Unlock()
}

// forgetRunner removes a runner from whichever scale set still has it. GitHub
// drops a JIT runner after its job, so this only matters for VMs that ended
// without one.
func forgetRunner(ctx context.Context, sets []*scaleSet, name string) error {
	for _, s := range sets {
		if s.client == nil {
			continue
		}
		if err := removeRunner(ctx, s.client, name); err != nil {
			return err
		}
	}
	return nil
}

// reconcile tears down VMs that powered off or are stuck and boots VMs for
// the scale sets with jobs waiting, taking turns, while the host admits
// them. Only the reconcile loop runs it.
func (p *chPool) reconcile(ctx context.Context) {
	p.retryForgets(ctx)
	p.mu.Lock()
	vms := p.snapshot()
	p.mu.Unlock()
	now := time.Now()
	for _, vm := range vms {
		active, err := p.vms.active(ctx, vm.unit)
		if err != nil {
			continue
		}
		if active {
			reason := vm.expired(now)
			if reason == "" {
				continue
			}
			p.logger.Warn("stopping VM", "runner", vm.runner, "reason", reason)
		}
		if err := p.teardown(ctx, vm); err != nil {
			p.logger.Warn("VM cleanup deferred", "runner", vm.runner, "error", err)
		}
	}
	// After a failed boot this pass boots no more; it only refreshes the
	// busy state.
	for failed := false; ; {
		h, probeErr := p.host.state(p.cfg.ch.workDir)
		now := time.Now()
		p.mu.Lock()
		running := p.counts()
		reason := p.admission(now, h, probeErr)
		var set *scaleSet
		if reason == "" && !failed {
			set = nextWaiting(p.sets, &p.next, running)
		}
		var vm *chVM
		if set != nil {
			var ok bool
			if vm, ok = p.reserve(set, now); !ok {
				reason = "every address is in use"
			}
		}
		if vm == nil {
			p.noteBusy(reason, running)
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
		if err := p.boot(ctx, vm); err != nil {
			set.logger.Warn("booting runner", "runner", vm.runner, "error", err)
			if err := p.teardown(context.WithoutCancel(ctx), vm); err != nil {
				p.logger.Warn("VM cleanup deferred", "runner", vm.runner, "error", err)
			}
			failed = true
		}
	}
}

// noteBusy logs changes of the admission state and of waiting jobs. The
// caller holds p.mu.
func (p *chPool) noteBusy(reason string, running map[*scaleSet]int) {
	if (reason != "") != (p.busyReason != "") {
		if reason != "" {
			p.logger.Warn("host busy, booting no new VMs", "reason", reason, "running", len(p.running))
		} else {
			p.logger.Info("host has room again", "running", len(p.running))
		}
	}
	p.busyReason = reason
	for _, set := range p.sets {
		if waiting := max(0, set.assigned-running[set]); waiting != set.waiting {
			set.logger.Info("jobs waiting for a VM", "waiting", waiting, "running", running[set])
			set.waiting = waiting
		}
	}
}

// admission decides from a host probe taken without the lock whether a VM
// may boot at now. The caller holds p.mu.
func (p *chPool) admission(now time.Time, h hostState, probeErr error) string {
	if len(p.running) >= p.addrs.size() {
		return "every address is in use"
	}
	if probeErr != nil {
		return probeErr.Error()
	}
	hostFull := !p.deniedAt.IsZero() && now.Sub(p.deniedAt) < hostFullHold
	return admit(p.cfg, h, hostFull, p.manifest.vmMemory().bootBytes, pendingBytes(p.snapshot()))
}

// reserve claims an address and a runner name for a VM of s, counted as
// running from now on; false when no address is free. The caller holds p.mu.
func (p *chPool) reserve(s *scaleSet, now time.Time) (*chVM, bool) {
	addr, ok := p.addrs.allocate()
	if !ok {
		return nil, false
	}
	runner := fmt.Sprintf("%s-%d-%s", p.cfg.name, addr.octet, randomSuffix())
	vm := &chVM{
		set:       s,
		runner:    runner,
		unit:      vmUnit(p.cfg.name, addr),
		dir:       filepath.Join(p.cfg.ch.workDir, runner),
		specPath:  filepath.Join(p.cfg.ch.workDir, runner+".json"),
		addr:      addr,
		startedAt: now,
		mem:       p.manifest.vmMemory(),
	}
	p.running[runner] = vm
	return vm, true
}

// boot starts a reserved VM.
func (p *chPool) boot(ctx context.Context, vm *chVM) error {
	// A leftover unit at this address would hold the tap and sockets.
	if err := p.vms.stop(ctx, vm.unit); err != nil {
		return err
	}
	jit, err := vm.set.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: vm.runner, WorkFolder: "_work"}, vm.set.id)
	if err != nil {
		return fmt.Errorf("generating JIT config: %w", err)
	}
	vm.mu.Lock()
	vm.jit = jit.EncodedJITConfig
	vm.mu.Unlock()
	if err := p.prepare(vm, buildVMSpec(p.cfg, p.manifest, vm.runner, vm.addr, p.cpus)); err != nil {
		return err
	}
	go serveControl(vm.ln, vm)
	if err := p.vms.boot(ctx, systemdRunArgv(vm.unit, p.self, vm.specPath)); err != nil {
		return err
	}
	vm.set.logger.Info("booting runner", "runner", vm.runner, "unit", vm.unit, "ip", vm.addr.ip(), "cid", vm.addr.cid())
	return nil
}

// prepare creates what a VM needs before its unit starts: its directory, an
// empty disk, its tap and the control socket, all usable by the VM user, and
// the spec its supervisor reads.
func (p *chPool) prepare(vm *chVM, spec vmSpec) error {
	uid, gid := p.account.uid, p.account.gid
	if err := os.Mkdir(vm.dir, 0700); err != nil {
		return err
	}
	if err := os.Chown(vm.dir, uid, gid); err != nil {
		return err
	}
	disk, err := os.OpenFile(diskImage(vm.dir), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = disk.Truncate(diskBytes)
	if err == nil {
		err = disk.Chown(uid, gid)
	}
	if closeErr := disk.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := p.net.createTap(vm.addr.tap(), uid, p.cfg.ch.bridge); err != nil {
		return err
	}
	socket := vsockPortSocket(vm.dir, p.cfg.ch.controlPort)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	vm.ln = ln
	if err := os.Chown(socket, uid, gid); err != nil {
		return err
	}
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(vm.specPath, raw, 0600)
}

// teardown stops a VM and removes everything it had; the VM stays counted
// until that succeeded.
func (p *chPool) teardown(ctx context.Context, vm *chVM) error {
	if err := p.vms.stop(ctx, vm.unit); err != nil {
		return err
	}
	if vm.ln != nil {
		vm.ln.Close()
	}
	if err := p.net.deleteTap(vm.addr.tap()); err != nil {
		return err
	}
	if err := os.RemoveAll(vm.dir); err != nil {
		return err
	}
	if err := os.Remove(vm.specPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	p.mu.Lock()
	sets := p.sets
	p.mu.Unlock()
	forgetErr := forgetRunner(ctx, sets, vm.runner)
	if forgetErr != nil {
		p.logger.Warn("runner cleanup deferred", "runner", vm.runner, "error", forgetErr)
	}
	vm.mu.Lock()
	p.logger.Info("runner finished", "runner", vm.runner, "org", vm.set.org, "lifetime", time.Since(vm.startedAt).Round(time.Second), "memoryGiB", float64(vm.mem.actualBytes)/float64(gib), "jitServed", vm.jitState == jitServed)
	vm.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if forgetErr != nil {
		p.forgets = append(p.forgets, vm.runner)
	}
	delete(p.running, vm.runner)
	p.addrs.release(vm.addr)
	return nil
}

// background runs the reconcile loop and the memory loop.
func (p *chPool) background(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.kick:
				p.reconcile(ctx)
			}
		}
	})
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			p.controlMemory(ctx, time.Now())
		}
	}
}

// controlMemory applies the memory policy to every VM, one at a time. Each
// decision and its grant happen under p.mu against what is pending right
// then, the same as admission; the resize request runs without it.
func (p *chPool) controlMemory(ctx context.Context, now time.Time) {
	p.mu.Lock()
	vms := p.snapshot()
	p.mu.Unlock()
	for _, vm := range vms {
		actual, err := p.vms.memory(ctx, vm.dir)
		if err != nil || actual <= 0 {
			continue
		}
		// Probed right before the decision, so host memory and pending
		// memory are read at about the same moment.
		h, err := p.host.state(p.cfg.ch.workDir)
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.running[vm.runner] != vm {
			p.mu.Unlock()
			continue
		}
		m := vm.memorySnapshot(actual)
		action, target := memoryDecision(m, hostMemory{available: h.memAvailable, reserve: memoryReserve(p.cfg, h.memTotal), pending: pendingBytes(p.snapshot())}, now)
		var undo func()
		firstDenial := false
		switch action {
		case memoryDenied:
			p.deniedAt = now
			firstDenial = vm.markDenied()
		case memoryPlug, memoryUnplug:
			undo = vm.grantResize(actual, target, now)
		}
		p.mu.Unlock()
		if firstDenial {
			p.logger.Warn("host full, guest memory not grown", "runner", vm.runner, "actualGiB", float64(actual)/float64(gib))
		}
		if undo == nil {
			continue
		}
		if err := p.vms.resize(ctx, vm.dir, target); err != nil {
			undo()
			p.logger.Warn("resizing guest memory", "runner", vm.runner, "error", err)
			continue
		}
		vm.resizeDone(latest(now, time.Now()))
		p.logger.Info("guest memory changed", "runner", vm.runner, "action", action.String(), "fromGiB", float64(actual)/float64(gib), "toGiB", float64(target)/float64(gib), "guestAvailableGiB", float64(m.stats.available())/float64(gib), "psiSome", m.stats.PSISomeAvg10)
	}
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// systemdVMs runs each VM's supervisor in a transient systemd unit and
// resizes it over the cloud-hypervisor API.
type systemdVMs struct{}

func (systemdVMs) boot(ctx context.Context, argv []string) error {
	if out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", argv[0], err, out)
	}
	return nil
}

func (systemdVMs) active(ctx context.Context, unit string) (bool, error) {
	return unitRunning(ctx, unit)
}

func (systemdVMs) stop(ctx context.Context, unit string) error {
	exec.CommandContext(ctx, "systemctl", "stop", unit).Run()
	exec.CommandContext(ctx, "systemctl", "reset-failed", unit).Run()
	running, err := unitRunning(ctx, unit)
	if err != nil {
		return err
	}
	if running {
		return fmt.Errorf("%s still running", unit)
	}
	return nil
}

func (systemdVMs) units(ctx context.Context, glob string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "list-units", "--all", "--plain", "--no-legend", "--type=service", glob).Output()
	if err != nil {
		return nil, err
	}
	var units []string
	for _, line := range strings.Split(string(out), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			units = append(units, fields[0])
		}
	}
	return units, nil
}

func (systemdVMs) memory(ctx context.Context, dir string) (int64, error) {
	info, err := newCHClient(apiSocket(dir)).info(ctx)
	return info.MemoryActualSize, err
}

func (systemdVMs) resize(ctx context.Context, dir string, desired int64) error {
	return newCHClient(apiSocket(dir)).resizeMemory(ctx, desired)
}

// procHost reads /proc and the work dir's file system.
type procHost struct{}

func (procHost) state(workDir string) (hostState, error) {
	h := hostState{cpus: runtime.NumCPU()}
	var err error
	if h.load1, err = load1(); err != nil {
		return h, err
	}
	mem, err := meminfoKiB()
	if err != nil {
		return h, err
	}
	h.memTotal, h.memAvailable = mem["MemTotal"]<<10, mem["MemAvailable"]<<10
	if h.memTotal == 0 {
		return h, errors.New("no MemTotal in /proc/meminfo")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(workDir, &fs); err != nil {
		return h, err
	}
	h.diskFree = int64(fs.Bavail) * fs.Bsize
	h.diskTotal = int64(fs.Blocks) * fs.Bsize
	return h, nil
}

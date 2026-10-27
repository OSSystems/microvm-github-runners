// microvm-scaleset runs GitHub Actions runner scale sets whose runners are
// throwaway microVMs. Every organization gets its own scale set, but all of
// them draw from one pool of VMs, so an idle organization leaves its share to
// the busy ones. The number of VMs grows with demand for as long as the host
// has spare CPU and memory, so a busy host stops taking new work instead of
// overcommitting.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/golang-jwt/jwt/v5"
)

type config struct {
	urls               []string
	name               string
	labels             string
	appID              string
	privateKeyFile     string
	slots              string
	jitDir             string
	jitOwner           string
	maxLoadPerCPU      float64
	vmMemGiB           float64
	vmInitialMemGiB    float64
	memReserveGiB      float64
	memCommitRatio     float64
	stateDir           string
	statusFile         string
	balloonEnabled     bool
	balloonFloorGiB    float64
	balloonHeadroomGiB float64
	balloonSlot        string
}

func parseConfig(args []string) (config, error) {
	var c config
	f := flag.NewFlagSet("microvm-scaleset", flag.ContinueOnError)
	f.Func("url", "organization URL; repeat to share the pool", func(v string) error { c.urls = append(c.urls, v); return nil })
	f.StringVar(&c.name, "name", "", "scale set name in every organization")
	f.StringVar(&c.labels, "labels", "", "comma-separated labels (defaults to the name)")
	f.StringVar(&c.appID, "app-id", "", "GitHub App ID")
	f.StringVar(&c.privateKeyFile, "private-key-file", "", "GitHub App private key")
	f.StringVar(&c.slots, "slots", "", "comma-separated microVM slot names")
	f.StringVar(&c.jitDir, "jit-dir", "/run/microvm-jit", "runner configuration and ownership directory")
	f.StringVar(&c.jitOwner, "jit-owner", "microvm", "user the hypervisor reads JIT configs as")
	f.StringVar(&c.stateDir, "state-dir", "/var/lib/microvms", "microvm.nix state directory")
	f.StringVar(&c.statusFile, "status-file", "/run/microvm-scaleset/status.json", "memory and ownership status file")
	f.Float64Var(&c.maxLoadPerCPU, "max-load-per-cpu", 1, "take no new jobs above this load per CPU")
	f.Float64Var(&c.vmMemGiB, "vm-mem", 10, "boot memory ceiling per VM, GiB")
	f.Float64Var(&c.vmInitialMemGiB, "vm-initial-mem", 0, "initial guest allocation, GiB; zero defaults to ceiling")
	f.Float64Var(&c.memReserveGiB, "mem-reserve", 8, "host memory reserve, GiB")
	f.Float64Var(&c.memCommitRatio, "mem-commit-ratio", 1, "nominal memory ceiling / usable host RAM; 2 means 200%")
	var oldOvercommit float64
	f.Float64Var(&oldOvercommit, "mem-overcommit", 0, "deprecated: extra memory fraction; use --mem-commit-ratio=1+fraction")
	f.BoolVar(&c.balloonEnabled, "balloon-enable", false, "reclaim guest memory under host pressure")
	f.Float64Var(&c.balloonFloorGiB, "balloon-floor", 8, "minimum balloon allocation, GiB")
	f.Float64Var(&c.balloonHeadroomGiB, "balloon-headroom", 2, "available memory retained inside each guest, GiB")
	f.StringVar(&c.balloonSlot, "balloon-slot", "", "restrict reclaim to this canary slot; empty selects all")
	if err := f.Parse(args); err != nil {
		return c, err
	}
	supplied := map[string]bool{}
	f.Visit(func(v *flag.Flag) { supplied[v.Name] = true })
	if supplied["mem-overcommit"] {
		if supplied["mem-commit-ratio"] {
			return c, errors.New("use only one of --mem-overcommit and --mem-commit-ratio")
		}
		c.memCommitRatio = 1 + oldOvercommit
	}
	if !supplied["vm-initial-mem"] {
		c.vmInitialMemGiB = c.vmMemGiB
	}
	for _, v := range []float64{c.vmInitialMemGiB, c.maxLoadPerCPU, c.vmMemGiB, c.memCommitRatio, c.balloonFloorGiB, c.balloonHeadroomGiB} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			return c, errors.New("memory and load settings must be finite and positive")
		}
	}
	if c.memCommitRatio < 1 || math.IsNaN(c.memReserveGiB) || math.IsInf(c.memReserveGiB, 0) || c.memReserveGiB < 0 {
		return c, errors.New("commit ratio must be at least 1 and reserve must be nonnegative")
	}
	if c.vmInitialMemGiB > c.vmMemGiB {
		return c, errors.New("initial memory exceeds VM ceiling")
	}
	if c.balloonEnabled && c.balloonFloorGiB > c.vmInitialMemGiB {
		return c, errors.New("balloon floor exceeds VM boot memory")
	}
	for _, slot := range strings.Split(c.slots, ",") {
		if slot != "" && (filepath.Base(slot) != slot || slot == "." || slot == ".." || strings.ContainsAny(slot, " \t\r\n")) {
			return c, errors.New("invalid VM slot name")
		}
	}
	return c, nil
}

func main() {
	c, err := parseConfig(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, logger, c); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger, c config) error {
	if len(c.urls) == 0 {
		return errors.New("no --url given")
	}
	lock, err := os.OpenFile(filepath.Join(c.jitDir, ".controller.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another controller owns the VM pool")
	}
	key, err := os.ReadFile(c.privateKeyFile)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	p := &pool{
		cfg:        c,
		logger:     logger,
		slots:      strings.Split(c.slots, ","),
		booting:    map[string]time.Time{},
		owners:     map[string]owner{},
		ownerRetry: map[string]time.Time{},
		samples:    map[string]vmSample{},
	}
	for _, url := range c.urls {
		s, err := p.register(ctx, url, key)
		if err != nil {
			return err
		}
		defer s.session.Close(context.WithoutCancel(ctx))
	}

	p.recoverOwners(ctx)
	if mem, e := meminfoGiB(); e == nil {
		capGiB := (mem["MemTotal"] - c.memReserveGiB) * c.memCommitRatio
		logger.Info("nominal VM capacity", "capGiB", capGiB, "maximumVMs", min(len(p.slots), int(capGiB/c.vmInitialMemGiB)), "poolSlots", len(p.slots))
	}
	logger.Info("memory policy", "ceilingGiB", c.vmMemGiB, "initialGiB", c.vmInitialMemGiB, "hostReserveGiB", c.memReserveGiB, "commitRatio", c.memCommitRatio, "poolSlots", len(p.slots), "balloonEnabled", c.balloonEnabled)
	var wg sync.WaitGroup
	wg.Go(func() { p.telemetryLoop(ctx) })
	for _, s := range p.sets {
		wg.Go(func() {
			cancel(fmt.Errorf("%s: %w", s.org, s.listener.Run(ctx, s)))
		})
	}
	// A listener only asks for runners when GitHub answers its long poll, so
	// slots another organization frees are handed out from here too.
	wg.Go(func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				p.mu.Lock()
				p.reconcile(ctx)
				p.mu.Unlock()
			}
		}
	})
	wg.Wait()
	return context.Cause(ctx)
}

// pool hands one set of microVMs to the scale sets of every organization.
type pool struct {
	cfg              config
	logger           *slog.Logger
	sets             []*scaleSet
	slots            []string
	mu               sync.Mutex
	busyReason       string
	unresolvedOwners bool
	// booting holds slots started recently; systemd reports them inactive
	// until their start job runs.
	booting map[string]time.Time
	// owners maps each slot to the scale set and runner it was last booted
	// with.
	owners     map[string]owner
	ownerRetry map[string]time.Time
	memoryMu   sync.Mutex
	controlMu  sync.Mutex
	samples    map[string]vmSample
	// next is the scale set that gets the next free slot, so a busy
	// organization cannot starve the others.
	next int
}

type owner struct {
	set    *scaleSet
	runner string
	record ownerRecord
	source string
}

type runnerClient interface {
	GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error)
	RemoveRunner(context.Context, int64) error
}

type scaleSet struct {
	pool     *pool
	org      string
	logger   *slog.Logger
	client   runnerClient
	id       int
	session  *scaleset.MessageSessionClient
	listener *listener.Listener
	// assigned is how many jobs GitHub last said this scale set has.
	assigned int
	// waiting is how many of them had no VM at the last reconcile.
	waiting int
}

func (p *pool) register(ctx context.Context, url string, key []byte) (*scaleSet, error) {
	org := filepath.Base(strings.TrimSuffix(url, "/"))
	logger := p.logger.With("org", org)
	installationID, err := installationID(ctx, p.cfg.appID, key, org)
	if err != nil {
		return nil, fmt.Errorf("looking up the App installation on %s: %w", org, err)
	}

	client, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: url,
		GitHubAppAuth: scaleset.GitHubAppAuth{
			ClientID:       p.cfg.appID,
			InstallationID: installationID,
			PrivateKey:     string(key),
		},
		SystemInfo: scaleset.SystemInfo{System: "microvm-scaleset"},
	})
	if err != nil {
		return nil, err
	}

	labels := []scaleset.Label{}
	for _, l := range strings.Split(p.cfg.labels, ",") {
		if l = strings.TrimSpace(l); l != "" {
			labels = append(labels, scaleset.Label{Name: l})
		}
	}
	want := &scaleset.RunnerScaleSet{
		Name:          p.cfg.name,
		RunnerGroupID: 1,
		Labels:        labels,
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	}
	set, err := client.GetRunnerScaleSet(ctx, 1, p.cfg.name)
	if err != nil {
		return nil, err
	}
	if set == nil {
		set, err = client.CreateRunnerScaleSet(ctx, want)
	} else {
		set, err = client.UpdateRunnerScaleSet(ctx, set.ID, want)
	}
	if err != nil {
		return nil, fmt.Errorf("registering scale set %s on %s: %w", p.cfg.name, org, err)
	}
	client.SetSystemInfo(scaleset.SystemInfo{System: "microvm-scaleset", ScaleSetID: set.ID})
	logger.Info("scale set ready", "id", set.ID, "name", set.Name)

	hostname, _ := os.Hostname()
	session, err := client.MessageSessionClient(ctx, set.ID, hostname)
	if err != nil {
		return nil, err
	}
	// Every scale set accepts as many jobs as the pool has VMs, even while the
	// host is busy: GitHub does not, in practice, offer a job again once it was offered to a
	// scale set without room, so a job turned away would stay queued forever.
	// Accepted jobs wait here instead until reconcile has room to boot them.
	l, err := listener.New(session, listener.Config{
		ScaleSetID: set.ID,
		MaxRunners: len(p.slots),
		Logger:     logger.WithGroup("listener"),
	})
	if err != nil {
		session.Close(context.WithoutCancel(ctx))
		return nil, err
	}

	s := &scaleSet{
		pool:     p,
		org:      org,
		logger:   logger,
		client:   client,
		id:       set.ID,
		session:  session,
		listener: l,
	}
	p.sets = append(p.sets, s)
	return s, nil
}

func (s *scaleSet) HandleJobStarted(_ context.Context, job *scaleset.JobStarted) error {
	s.logger.Info("job started", "runner", job.RunnerName, "job", job.JobID)
	return nil
}

func (s *scaleSet) HandleJobCompleted(_ context.Context, job *scaleset.JobCompleted) error {
	s.logger.Info("job completed", "runner", job.RunnerName, "job", job.JobID, "result", job.Result)
	return nil
}

// HandleDesiredRunnerCount records how many jobs GitHub assigned to this
// scale set, boots microVMs for them while the host has room and reports how
// many runners the scale set has. Called at least once per long poll, so it
// also notices VMs that powered off.
func (s *scaleSet) HandleDesiredRunnerCount(ctx context.Context, assigned int) (int, error) {
	p := s.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	s.assigned = assigned
	return p.reconcile(ctx)[s], nil
}

// reconcile frees the slots whose VM powered off and boots VMs for the scale
// sets with jobs still waiting, taking turns between them, while the host has
// room. It returns how many runners each scale set has. The caller holds p.mu.
func (p *pool) reconcile(ctx context.Context) map[*scaleSet]int {
	running := map[*scaleSet]int{}
	var free []string
	unknown := 0
	for _, slot := range p.slots {
		active := vmRunning(slot)
		if _, ok := p.owners[slot]; !ok && time.Now().After(p.ownerRetry[slot]) {
			if err := p.restoreOwner(ctx, slot, active); err != nil {
				p.logger.Warn("runner ownership unresolved", "slot", slot, "error", err)
				if p.ownerRetry == nil {
					p.ownerRetry = map[string]time.Time{}
				}
				p.ownerRetry[slot] = time.Now().Add(time.Minute)
			}
		}
		occupied := active
		if active {
			delete(p.booting, slot)
		} else if t, ok := p.booting[slot]; ok && time.Since(t) < bootGrace {
			occupied = true
		}
		if occupied {
			if o, ok := p.owners[slot]; ok {
				running[o.set]++
			} else {
				unknown++
			}
			continue
		}
		delete(p.booting, slot)
		if !p.forget(ctx, slot) {
			if o, ok := p.owners[slot]; ok {
				running[o.set]++
			} else {
				unknown++
			}
			continue
		}
		delete(p.ownerRetry, slot)
		free = append(free, slot)
	}
	p.unresolvedOwners = unknown > 0
	room, reason := p.room(len(p.slots) - len(free))
	if unknown > 0 {
		room = 0
		reason = fmt.Sprintf("%d active slots have unknown owners", unknown)
	}
	if (reason != "") != (p.busyReason != "") {
		if reason != "" {
			p.logger.Warn("host busy, booting no new VMs", "reason", reason, "running", len(p.slots)-len(free))
		} else {
			p.logger.Info("host has room again", "running", len(p.slots)-len(free))
		}
	}
	p.busyReason = reason
	room = min(room, len(free))
	for room > 0 {
		var set *scaleSet
		for i := range p.sets {
			candidate := p.sets[(p.next+i)%len(p.sets)]
			if running[candidate] < candidate.assigned {
				set = candidate
				p.next = (p.next + i + 1) % len(p.sets)
				break
			}
		}
		if set == nil {
			break
		}
		slot := free[0]
		if err := p.boot(ctx, set, slot); err != nil {
			set.logger.Warn("booting runner", "slot", slot, "error", err)
			break
		}
		free = free[1:]
		running[set]++
		// Reserve newly requested memory before admitting another boot.
		room, _ = p.room(len(p.slots) - len(free))
		room = min(room, len(free))
	}
	for _, set := range p.sets {
		if waiting := max(0, set.assigned-running[set]); waiting != set.waiting {
			set.logger.Info("jobs waiting for a VM", "waiting", waiting, "running", running[set])
			set.waiting = waiting
		}
	}
	p.writeStatus(running, unknown)
	return running
}

func (p *pool) boot(ctx context.Context, s *scaleSet, slot string) error {
	suffix := make([]byte, 4)
	rand.Read(suffix)
	name := slot + "-" + hex.EncodeToString(suffix)

	record := ownerRecord{Version: 1, Org: s.org, ScaleSetID: s.id, Runner: name, StartedAt: time.Now(), MemoryGiB: p.cfg.vmMemGiB, InitialMemoryGiB: p.cfg.vmInitialMemGiB}
	if err := p.writeOwner(slot, record); err != nil {
		return err
	}
	p.owners[slot] = owner{set: s, runner: name, record: record, source: "boot"}
	p.booting[slot] = record.StartedAt
	jit, err := s.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{
		Name:       name,
		WorkFolder: "_work",
	}, s.id)
	if err != nil {
		return fmt.Errorf("generating JIT config for %s: %w", name, err)
	}

	path := filepath.Join(p.cfg.jitDir, slot, "jitconfig")
	if err := os.WriteFile(path+".tmp", []byte(jit.EncodedJITConfig), 0o400); err != nil {
		return err
	}
	if err := chown(path+".tmp", p.cfg.jitOwner); err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}

	if err := os.Remove(filepath.Join(p.cfg.jitDir, slot, "control", "ready")); err != nil && !os.IsNotExist(err) {
		return err
	}
	unit := "microvm@" + slot + ".service"
	exec.Command("systemctl", "reset-failed", unit).Run()
	// virtiofsd exits with the previous VM and its supervisor does not bring
	// it back, so every boot needs a fresh one.
	virtiofsd := "microvm-virtiofsd@" + slot + ".service"
	if out, err := exec.CommandContext(ctx, "systemctl", "restart", virtiofsd).CombinedOutput(); err != nil {
		return fmt.Errorf("restarting %s: %w: %s", virtiofsd, err, out)
	}
	if out, err := exec.Command("systemctl", "start", "--no-block", unit).CombinedOutput(); err != nil {
		return fmt.Errorf("starting %s: %w: %s", unit, err, out)
	}
	p.booting[slot] = time.Now()
	p.owners[slot] = owner{set: s, runner: name, record: record, source: "boot"}
	s.logger.Info("booting runner", "slot", slot, "runner", name)
	return nil
}

// forget removes the registration of a slot's last runner. GitHub drops a
// runner once it finished its job, so this only matters for VMs that died
// before taking one.
func (p *pool) forget(ctx context.Context, slot string) bool {
	if o, ok := p.owners[slot]; ok {
		lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		runner, err := o.set.client.GetRunnerByName(lookupCtx, o.runner)
		if err != nil {
			p.logger.Warn("runner cleanup deferred", "slot", slot, "error", err)
			return false
		}
		if runner != nil {
			if err := o.set.client.RemoveRunner(lookupCtx, int64(runner.ID)); err != nil {
				p.logger.Warn("runner cleanup deferred", "slot", slot, "error", err)
				return false
			}
		}
	}
	for _, name := range []string{"jitconfig", "owner.json"} {
		if err := os.Remove(filepath.Join(p.cfg.jitDir, slot, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			p.logger.Warn("slot cleanup deferred", "slot", slot, "error", err)
			return false
		}
	}
	if o, ok := p.owners[slot]; ok {
		sample := p.memorySnapshot()[slot]
		if sample.Runner == o.runner {
			p.logger.Info("runner memory summary", "slot", slot, "org", o.set.org, "runner", o.runner, "observations", sample.Observations, "peakGuestUsedGiB", float64(sample.PeakUsedBytes)/float64(gib), "peakResidentGiB", float64(sample.PeakResidentBytes)/float64(gib), "oomKills", sample.Guest.OOMKills)
		}
	}
	delete(p.owners, slot)
	p.memoryMu.Lock()
	delete(p.samples, slot)
	p.memoryMu.Unlock()
	return true
}

func chown(path, owner string) error {
	u, err := user.Lookup(owner)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return os.Chown(path, uid, gid)
}

// vmRunning reports whether the slot's VM is running.
func vmRunning(slot string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "show", "--property=ActiveState", "--value", "microvm@"+slot+".service").Output()
	if err != nil {
		return true
	}
	switch strings.TrimSpace(string(out)) {
	case "active", "activating", "deactivating":
		return true
	}
	job, err := exec.CommandContext(ctx, "systemctl", "show", "--property=Job", "--value", "microvm@"+slot+".service").Output()
	if err != nil {
		return true
	}
	value := strings.TrimSpace(string(job))
	return value != "" && value != "0" && value != "[0 /]"
}

// room returns how many more VMs fit next to the running ones, or 0 and why
// the host should take no new jobs. The VMs' memory may add up to more than
// the host has, by memOvercommit: they rarely all use what they were given,
// return what they free, and zram absorbs a peak. Memory that is actually in
// use still has to leave room for one more VM.
func (p *pool) room(running int) (int, string) {
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		if load, err := strconv.ParseFloat(strings.Fields(string(raw))[0], 64); err == nil {
			if perCPU := load / float64(runtime.NumCPU()); perCPU > p.cfg.maxLoadPerCPU {
				return 0, fmt.Sprintf("load %.2f per CPU", perCPU)
			}
		}
	}
	mem, err := meminfoGiB()
	if err != nil {
		return 0, err.Error()
	}
	return p.memoryRoom(running, mem)
}

func (p *pool) memoryRoom(running int, mem map[string]float64) (int, string) {
	usable := mem["MemTotal"] - p.cfg.memReserveGiB
	given := float64(running) * p.cfg.vmMemGiB
	if len(p.owners) > 0 {
		given = 0
		samples := p.memorySnapshot()
		for slot, o := range p.owners {
			given += committedMemoryGiB(o, samples[slot])
		}
		given += float64(max(0, running-len(p.owners))) * p.cfg.vmMemGiB
	}
	initial := p.cfg.vmInitialMemGiB
	if initial <= 0 {
		initial = p.cfg.vmMemGiB
	}
	n := int((usable*p.cfg.memCommitRatio - given) / initial)
	if n <= 0 {
		return 0, fmt.Sprintf("%.1f GiB given to %d VMs, %.0f%% of the %.1f GiB usable", given, running, given/usable*100, usable)
	}
	growth, emergency := p.growthReservation(time.Now())
	if emergency {
		return 0, "guest memory emergency; awaiting safe growth"
	}
	availableSlots := int((mem["MemAvailable"] - p.cfg.memReserveGiB - growth) / initial)
	if availableSlots <= 0 {
		return 0, fmt.Sprintf("%.1f GiB available, %.1f GiB reserved for VM growth", mem["MemAvailable"], growth)
	}
	return min(n, availableSlots), ""
}

// meminfoGiB returns the fields of /proc/meminfo, in GiB.
func meminfoGiB() (map[string]float64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	mem := map[string]float64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 {
			kib, _ := strconv.ParseFloat(fields[1], 64)
			mem[strings.TrimSuffix(fields[0], ":")] = kib / (1 << 20)
		}
	}
	return mem, sc.Err()
}

// installationID finds the App's installation on org, so only the App ID and
// key need configuring.
func installationID(ctx context.Context, appID string, key []byte, org string) (int64, error) {
	signingKey, err := jwt.ParseRSAPrivateKeyFromPEM(key)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    appID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute)),
	}).SignedString(signingKey)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/orgs/"+org+"/installation", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GitHub returned %s", resp.Status)
	}
	var body struct {
		ID int64 `json:"id"`
	}
	return body.ID, json.NewDecoder(resp.Body).Decode(&body)
}

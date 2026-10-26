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
	urls           []string
	name           string
	labels         string
	appID          string
	privateKeyFile string
	slots          string
	jitDir         string
	jitOwner       string
	maxLoadPerCPU  float64
	vmMemGiB       float64
	memReserveGiB  float64
	memOvercommit  float64
}

func main() {
	var c config
	flag.Func("url", "organization URL, e.g. https://github.com/shellhub-io; repeat to share the pool between organizations", func(v string) error {
		c.urls = append(c.urls, v)
		return nil
	})
	flag.StringVar(&c.name, "name", "", "scale set name, the same in every organization")
	flag.StringVar(&c.labels, "labels", "", "comma-separated labels (defaults to the name)")
	flag.StringVar(&c.appID, "app-id", "", "GitHub App ID")
	flag.StringVar(&c.privateKeyFile, "private-key-file", "", "GitHub App private key")
	flag.StringVar(&c.slots, "slots", "", "comma-separated pool of microVM names to run jobs in")
	flag.StringVar(&c.jitDir, "jit-dir", "/run/microvm-jit", "directory holding one JIT config per microVM")
	flag.StringVar(&c.jitOwner, "jit-owner", "microvm", "user the hypervisor reads the JIT config as")
	flag.Float64Var(&c.maxLoadPerCPU, "max-load-per-cpu", 1.0, "take no new jobs above this 1-minute load average per CPU")
	flag.Float64Var(&c.vmMemGiB, "vm-mem", 10, "memory of one microVM, in GiB")
	flag.Float64Var(&c.memReserveGiB, "mem-reserve", 8, "memory to always leave for the host, in GiB")
	flag.Float64Var(&c.memOvercommit, "mem-overcommit", 0, "how far the VMs' memory may exceed the host's, e.g. 0.3 for 30%")
	flag.Parse()

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
	key, err := os.ReadFile(c.privateKeyFile)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	p := &pool{
		cfg:     c,
		logger:  logger,
		slots:   strings.Split(c.slots, ","),
		booting: map[string]time.Time{},
		owners:  map[string]owner{},
	}
	for _, url := range c.urls {
		s, err := p.register(ctx, url, key)
		if err != nil {
			return err
		}
		defer s.session.Close(context.WithoutCancel(ctx))
	}

	var wg sync.WaitGroup
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
	cfg        config
	logger     *slog.Logger
	sets       []*scaleSet
	slots      []string
	mu         sync.Mutex
	busyReason string
	// booting holds slots started recently; systemd reports them inactive
	// until their start job runs.
	booting map[string]time.Time
	// owners maps each slot to the scale set and runner it was last booted
	// with.
	owners map[string]owner
	// next is the scale set that gets the next free slot, so a busy
	// organization cannot starve the others.
	next int
}

type owner struct {
	set    *scaleSet
	runner string
}

type scaleSet struct {
	pool     *pool
	org      string
	logger   *slog.Logger
	client   *scaleset.Client
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
	for _, slot := range p.slots {
		ok := vmRunning(slot)
		if ok {
			delete(p.booting, slot)
		} else if t, pending := p.booting[slot]; pending && time.Since(t) < 2*time.Minute {
			ok = true
		}
		if ok {
			running[p.owners[slot].set]++
		} else {
			delete(p.booting, slot)
			p.forget(ctx, slot)
			free = append(free, slot)
			os.Remove(filepath.Join(p.cfg.jitDir, slot, "jitconfig"))
		}
	}

	room, reason := p.room(len(p.slots) - len(free))
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
		var s *scaleSet
		for i := range p.sets {
			c := p.sets[(p.next+i)%len(p.sets)]
			if running[c] < c.assigned {
				s = c
				p.next = (p.next + i + 1) % len(p.sets)
				break
			}
		}
		if s == nil {
			break
		}
		slot := free[0]
		if err := p.boot(ctx, s, slot); err != nil {
			s.logger.Warn("booting runner", "slot", slot, "error", err)
			break
		}
		free = free[1:]
		running[s]++
		room--
	}

	for _, s := range p.sets {
		if waiting := max(0, s.assigned-running[s]); waiting != s.waiting {
			s.logger.Info("jobs waiting for a VM", "waiting", waiting, "running", running[s])
			s.waiting = waiting
		}
	}
	return running
}

func (p *pool) boot(ctx context.Context, s *scaleSet, slot string) error {
	suffix := make([]byte, 4)
	rand.Read(suffix)
	name := slot + "-" + hex.EncodeToString(suffix)

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
	p.owners[slot] = owner{set: s, runner: name}
	s.logger.Info("booting runner", "slot", slot, "runner", name)
	return nil
}

// forget removes the registration of a slot's last runner. GitHub drops a
// runner once it finished its job, so this only matters for VMs that died
// before taking one.
func (p *pool) forget(ctx context.Context, slot string) {
	o, ok := p.owners[slot]
	if !ok {
		return
	}
	delete(p.owners, slot)
	runner, err := o.set.client.GetRunnerByName(ctx, o.runner)
	if err != nil || runner == nil {
		return
	}
	if err := o.set.client.RemoveRunner(ctx, int64(runner.ID)); err != nil {
		o.set.logger.Warn("removing unused runner", "runner", o.runner, "error", err)
		return
	}
	o.set.logger.Info("removed unused runner", "runner", o.runner)
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
	out, _ := exec.Command("systemctl", "show", "--property=ActiveState", "--value", "microvm@"+slot+".service").Output()
	switch strings.TrimSpace(string(out)) {
	case "active", "activating", "deactivating":
		return true
	}
	return false
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
	n := int((usable*(1+p.cfg.memOvercommit) - given) / p.cfg.vmMemGiB)
	if n <= 0 {
		return 0, fmt.Sprintf("%.1f GiB given to %d VMs, %.0f%% of the %.1f GiB usable", given, running, given/usable*100, usable)
	}
	availableSlots := int((mem["MemAvailable"] - p.cfg.memReserveGiB) / p.cfg.vmMemGiB)
	if availableSlots <= 0 {
		return 0, fmt.Sprintf("%.1f GiB available", mem["MemAvailable"])
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

// microvm-scaleset runs a GitHub Actions runner scale set whose runners are
// throwaway microVMs. The number of VMs grows with demand for as long as the
// host has spare CPU and memory, so a busy host stops taking new work
// instead of overcommitting.
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
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/golang-jwt/jwt/v5"
)

type config struct {
	url            string
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
}

func main() {
	var c config
	flag.StringVar(&c.url, "url", "", "organization URL, e.g. https://github.com/shellhub-io")
	flag.StringVar(&c.name, "name", "", "scale set name")
	flag.StringVar(&c.labels, "labels", "", "comma-separated labels (defaults to the name)")
	flag.StringVar(&c.appID, "app-id", "", "GitHub App ID")
	flag.StringVar(&c.privateKeyFile, "private-key-file", "", "GitHub App private key")
	flag.StringVar(&c.slots, "slots", "", "comma-separated pool of microVM names to run jobs in")
	flag.StringVar(&c.jitDir, "jit-dir", "/run/microvm-jit", "directory holding one JIT config per microVM")
	flag.StringVar(&c.jitOwner, "jit-owner", "microvm", "user the hypervisor reads the JIT config as")
	flag.Float64Var(&c.maxLoadPerCPU, "max-load-per-cpu", 1.0, "take no new jobs above this 1-minute load average per CPU")
	flag.Float64Var(&c.vmMemGiB, "vm-mem", 10, "memory of one microVM, in GiB")
	flag.Float64Var(&c.memReserveGiB, "mem-reserve", 8, "memory to always leave for the host, in GiB")
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
	key, err := os.ReadFile(c.privateKeyFile)
	if err != nil {
		return err
	}
	org := filepath.Base(strings.TrimSuffix(c.url, "/"))
	installationID, err := installationID(ctx, c.appID, key, org)
	if err != nil {
		return fmt.Errorf("looking up the App installation: %w", err)
	}

	client, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: c.url,
		GitHubAppAuth: scaleset.GitHubAppAuth{
			ClientID:       c.appID,
			InstallationID: installationID,
			PrivateKey:     string(key),
		},
		SystemInfo: scaleset.SystemInfo{System: "microvm-scaleset"},
	})
	if err != nil {
		return err
	}

	labels := []scaleset.Label{}
	for _, l := range strings.Split(c.labels, ",") {
		if l = strings.TrimSpace(l); l != "" {
			labels = append(labels, scaleset.Label{Name: l})
		}
	}
	want := &scaleset.RunnerScaleSet{
		Name:          c.name,
		RunnerGroupID: 1,
		Labels:        labels,
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	}
	scaleSet, err := client.GetRunnerScaleSet(ctx, 1, c.name)
	if err != nil {
		return err
	}
	if scaleSet == nil {
		scaleSet, err = client.CreateRunnerScaleSet(ctx, want)
	} else {
		scaleSet, err = client.UpdateRunnerScaleSet(ctx, scaleSet.ID, want)
	}
	if err != nil {
		return fmt.Errorf("registering scale set %s: %w", c.name, err)
	}
	client.SetSystemInfo(scaleset.SystemInfo{System: "microvm-scaleset", ScaleSetID: scaleSet.ID})
	logger.Info("scale set ready", "id", scaleSet.ID, "name", scaleSet.Name)

	hostname, _ := os.Hostname()
	session, err := client.MessageSessionClient(ctx, scaleSet.ID, hostname)
	if err != nil {
		return err
	}
	defer session.Close(context.WithoutCancel(ctx))

	slots := strings.Split(c.slots, ",")
	l, err := listener.New(session, listener.Config{
		ScaleSetID: scaleSet.ID,
		MaxRunners: len(slots),
		Logger:     logger.WithGroup("listener"),
	})
	if err != nil {
		return err
	}

	s := &scaler{
		cfg:        c,
		logger:     logger,
		client:     client,
		scaleSetID: scaleSet.ID,
		slots:      slots,
		listener:   l,
		booting:    map[string]time.Time{},
		runners:    map[string]string{},
	}
	return l.Run(ctx, s)
}

type scaler struct {
	cfg        config
	logger     *slog.Logger
	client     *scaleset.Client
	scaleSetID int
	slots      []string
	listener   *listener.Listener
	busyReason string
	capacity   int
	// booting holds slots started recently; systemd reports them inactive
	// until their start job runs.
	booting map[string]time.Time
	// runners maps each slot to the runner it was last booted with.
	runners map[string]string
}

func (s *scaler) HandleJobStarted(_ context.Context, job *scaleset.JobStarted) error {
	s.logger.Info("job started", "runner", job.RunnerName, "job", job.JobID)
	return nil
}

func (s *scaler) HandleJobCompleted(_ context.Context, job *scaleset.JobCompleted) error {
	s.logger.Info("job completed", "runner", job.RunnerName, "job", job.JobID, "result", job.Result)
	return nil
}

// HandleDesiredRunnerCount boots a microVM per assigned job while the host
// has room, and reports how many runners the host can take right now. Called at
// least once per long poll, so it also notices VMs that powered off.
func (s *scaler) HandleDesiredRunnerCount(ctx context.Context, assigned int) (int, error) {
	var running, free []string
	var growth float64
	for _, slot := range s.slots {
		used, ok := vmMemoryGiB(slot)
		if ok {
			delete(s.booting, slot)
		} else if t, pending := s.booting[slot]; pending && time.Since(t) < 2*time.Minute {
			ok = true
		}
		if ok {
			running = append(running, slot)
			growth += max(0, s.cfg.vmMemGiB-used)
		} else {
			delete(s.booting, slot)
			s.forget(ctx, slot)
			free = append(free, slot)
			os.Remove(filepath.Join(s.cfg.jitDir, slot, "jitconfig"))
		}
	}

	room, reason := s.room(growth)
	capacity := len(running) + min(room, len(free))
	if reason != s.busyReason {
		if reason != "" {
			s.logger.Warn("host busy, taking no new jobs", "reason", reason, "running", len(running))
		} else {
			s.logger.Info("host has room again", "running", len(running))
		}
		s.busyReason = reason
	}
	if capacity != s.capacity {
		s.logger.Info("capacity", "runners", capacity, "running", len(running), "assigned", assigned)
		s.capacity = capacity
	}
	s.listener.SetMaxRunners(capacity)

	for _, slot := range free {
		if len(running) >= min(assigned, capacity) {
			break
		}
		if err := s.boot(ctx, slot); err != nil {
			return len(running), err
		}
		running = append(running, slot)
	}
	return len(running), nil
}

func (s *scaler) boot(ctx context.Context, slot string) error {
	suffix := make([]byte, 4)
	rand.Read(suffix)
	name := slot + "-" + hex.EncodeToString(suffix)

	jit, err := s.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{
		Name:       name,
		WorkFolder: "_work",
	}, s.scaleSetID)
	if err != nil {
		return fmt.Errorf("generating JIT config for %s: %w", name, err)
	}

	path := filepath.Join(s.cfg.jitDir, slot, "jitconfig")
	if err := os.WriteFile(path+".tmp", []byte(jit.EncodedJITConfig), 0o400); err != nil {
		return err
	}
	if err := chown(path+".tmp", s.cfg.jitOwner); err != nil {
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
	s.booting[slot] = time.Now()
	s.runners[slot] = name
	s.logger.Info("booting runner", "slot", slot, "runner", name)
	return nil
}

// vmMemoryGiB reports whether the slot's VM is running and how much memory
// it holds so far.
// forget removes the registration of a slot's last runner. GitHub drops a
// runner once it finished its job, so this only matters for VMs that died
// before taking one.
func (s *scaler) forget(ctx context.Context, slot string) {
	name, ok := s.runners[slot]
	if !ok {
		return
	}
	delete(s.runners, slot)
	runner, err := s.client.GetRunnerByName(ctx, name)
	if err != nil || runner == nil {
		return
	}
	if err := s.client.RemoveRunner(ctx, int64(runner.ID)); err != nil {
		s.logger.Warn("removing unused runner", "runner", name, "error", err)
		return
	}
	s.logger.Info("removed unused runner", "runner", name)
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

func vmMemoryGiB(slot string) (float64, bool) {
	out, _ := exec.Command("systemctl", "show", "--property=ActiveState,MemoryCurrent", "microvm@"+slot+".service").Output()
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			props[k] = v
		}
	}
	switch props["ActiveState"] {
	case "active", "activating", "deactivating":
	default:
		return 0, false
	}
	bytes, _ := strconv.ParseFloat(props["MemoryCurrent"], 64)
	return bytes / (1 << 30), true
}

// room returns how many more VMs fit, counting the memory running VMs may
// still grow into, or 0 and why the host should take no new jobs.
func (s *scaler) room(growth float64) (int, string) {
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		if load, err := strconv.ParseFloat(strings.Fields(string(raw))[0], 64); err == nil {
			if perCPU := load / float64(runtime.NumCPU()); perCPU > s.cfg.maxLoadPerCPU {
				return 0, fmt.Sprintf("load %.2f per CPU", perCPU)
			}
		}
	}
	avail, err := memAvailableGiB()
	if err != nil {
		return 0, err.Error()
	}
	n := int((avail - growth - s.cfg.memReserveGiB) / s.cfg.vmMemGiB)
	if n <= 0 {
		return 0, fmt.Sprintf("%.1f GiB available, %.1f GiB promised to running VMs", avail, growth)
	}
	return n, ""
}

func memAvailableGiB() (float64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kib, err := strconv.ParseFloat(fields[1], 64)
			return kib / (1 << 20), err
		}
	}
	return 0, errors.New("MemAvailable not found")
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

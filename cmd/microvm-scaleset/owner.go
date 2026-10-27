package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const bootGrace = 2 * time.Minute

type ownerRecord struct {
	Version    int       `json:"version"`
	Org        string    `json:"org"`
	ScaleSetID int       `json:"scaleSetId"`
	Runner     string    `json:"runner"`
	StartedAt  time.Time `json:"startedAt"`
	MemoryGiB  float64   `json:"memoryGiB"`
}

type runnerIdentity struct {
	AgentID    json.Number `json:"AgentId"`
	AgentName  string      `json:"AgentName"`
	ScaleSetID json.Number `json:"RunnerScaleSetId"`
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return b, nil
}

func readRunnerIdentity(path string) (runnerIdentity, error) {
	var r runnerIdentity
	encoded, err := readBounded(path, 4<<20)
	if err != nil {
		return r, errors.New("cannot read runner identity")
	}
	outer, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		return r, errors.New("invalid encoded runner identity")
	}
	var files map[string]json.RawMessage
	if json.Unmarshal(outer, &files) != nil {
		return r, errors.New("invalid runner configuration")
	}
	var runner string
	if json.Unmarshal(files[".runner"], &runner) != nil {
		return r, errors.New("missing runner identity")
	}
	raw, err := base64.StdEncoding.DecodeString(runner)
	if err != nil || json.Unmarshal(raw, &r) != nil {
		return r, errors.New("invalid runner identity")
	}
	id, e1 := r.AgentID.Int64()
	setID, e2 := r.ScaleSetID.Int64()
	if r.AgentName == "" || e1 != nil || e2 != nil || id <= 0 || setID <= 0 {
		return r, errors.New("incomplete runner identity")
	}
	return r, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (p *pool) writeOwner(slot string, r ownerRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(p.cfg.jitDir, slot, "owner.json"), b, 0600)
}

func (p *pool) readOwner(slot string) (ownerRecord, error) {
	var r ownerRecord
	b, err := readBounded(filepath.Join(p.cfg.jitDir, slot, "owner.json"), 4096)
	if err != nil {
		return r, err
	}
	if json.Unmarshal(b, &r) != nil || r.Version != 1 || r.Org == "" || r.ScaleSetID <= 0 || !strings.HasPrefix(r.Runner, slot+"-") || r.StartedAt.IsZero() || r.StartedAt.After(time.Now().Add(time.Minute)) || math.IsNaN(r.MemoryGiB) || math.IsInf(r.MemoryGiB, 0) || r.MemoryGiB <= 0 {
		return r, errors.New("invalid owner record")
	}
	return r, nil
}

func (p *pool) restoreOwner(ctx context.Context, slot string, active bool) error {
	if _, ok := p.owners[slot]; ok {
		return nil
	}
	r, err := p.readOwner(slot)
	if err == nil {
		if active {
			ref, e := readRunnerIdentity(filepath.Join(p.cfg.jitDir, slot, "jitconfig"))
			id, _ := ref.ScaleSetID.Int64()
			if e != nil || ref.AgentName != r.Runner || id != int64(r.ScaleSetID) {
				return errors.New("owner record does not match active runner")
			}
		}
		for _, s := range p.sets {
			if s.org == r.Org && s.id == r.ScaleSetID {
				p.owners[slot] = owner{set: s, runner: r.Runner, record: r, source: "record"}
				if time.Since(r.StartedAt) < bootGrace {
					p.booting[slot] = r.StartedAt
				}
				p.logger.Info("recovered runner ownership", "slot", slot, "org", s.org, "runner", r.Runner, "source", "record")
				return nil
			}
		}
		return errors.New("owner organization or scale set is no longer configured")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot load owner record")
	}
	if !active {
		return nil
	}
	ref, err := readRunnerIdentity(filepath.Join(p.cfg.jitDir, slot, "jitconfig"))
	if err != nil {
		return err
	}
	if !strings.HasPrefix(ref.AgentName, slot+"-") {
		return errors.New("runner identity does not match slot")
	}
	agentID, _ := ref.AgentID.Int64()
	setID, _ := ref.ScaleSetID.Int64()
	lookupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var match *scaleSet
	for _, s := range p.sets {
		if int64(s.id) != setID {
			continue
		}
		if s.client == nil {
			return errors.New("runner lookup unavailable")
		}
		got, e := s.client.GetRunnerByName(lookupCtx, ref.AgentName)
		if e != nil {
			return errors.New("organization runner lookup failed")
		}
		if got != nil && got.Name == ref.AgentName && int64(got.ID) == agentID && int64(got.RunnerScaleSetID) == setID {
			if match != nil {
				return errors.New("runner identity matches multiple organizations")
			}
			match = s
		}
	}
	if match == nil {
		return errors.New("runner has no confirmed organization")
	}
	q, err := openQMP(ctx, p.socketPath(slot))
	if err != nil {
		return fmt.Errorf("reading existing VM size: %w", err)
	}
	ceiling, err := q.memoryCeiling()
	q.Close()
	if err != nil {
		return err
	}
	r = ownerRecord{Version: 1, Org: match.org, ScaleSetID: match.id, Runner: ref.AgentName, StartedAt: time.Now(), MemoryGiB: float64(ceiling) / float64(gib)}
	if err = p.writeOwner(slot, r); err != nil {
		return err
	}
	p.owners[slot] = owner{set: match, runner: r.Runner, record: r, source: "legacy"}
	p.logger.Info("recovered runner ownership", "slot", slot, "org", match.org, "runner", r.Runner, "source", "legacy")
	return nil
}

func (p *pool) recoverOwners(ctx context.Context) {
	for _, slot := range p.slots {
		active := vmRunning(slot)
		if err := p.restoreOwner(ctx, slot, active); err != nil && active {
			p.logger.Warn("runner ownership unresolved; new boots inhibited", "slot", slot, "error", err)
			p.ownerRetry[slot] = time.Now().Add(time.Minute)
		}
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"time"
)

const gib int64 = 1 << 30
const balloonPath = "/machine/peripheral/balloon0"

type qmpClient struct {
	net.Conn
	decoder *json.Decoder
	encoder *json.Encoder
	nextID  int
}

type guestMemory struct {
	ActualBytes    int64 `json:"actualBytes"`
	CeilingBytes   int64 `json:"ceilingBytes"`
	AvailableBytes int64 `json:"availableBytes"`
	CacheBytes     int64 `json:"cacheBytes"`
	OOMKills       int64 `json:"oomKills"`
	UpdatedAt      int64 `json:"updatedAt"`
}

func (m guestMemory) fresh(now time.Time) bool {
	age := now.Unix() - m.UpdatedAt
	return m.UpdatedAt > 0 && age >= -1 && age <= 15 && m.ActualBytes > 0 && m.CeilingBytes >= m.ActualBytes && m.AvailableBytes >= 0 && m.AvailableBytes <= m.ActualBytes && m.OOMKills >= 0
}

func (p *pool) socketPath(slot string) string {
	return filepath.Join(p.cfg.stateDir, slot, slot+".sock")
}

func openQMP(ctx context.Context, path string) (*qmpClient, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	conn.SetDeadline(deadline)
	q := &qmpClient{Conn: conn, decoder: json.NewDecoder(io.LimitReader(conn, 1<<20)), encoder: json.NewEncoder(conn)}
	var greeting struct {
		QMP json.RawMessage `json:"QMP"`
	}
	if err = q.decoder.Decode(&greeting); err != nil || len(greeting.QMP) == 0 {
		conn.Close()
		return nil, errors.New("invalid QMP greeting")
	}
	if err = q.command("qmp_capabilities", nil, nil); err != nil {
		conn.Close()
		return nil, err
	}
	return q, nil
}

func (q *qmpClient) command(execute string, args any, out any) error {
	q.nextID++
	request := struct {
		Execute   string `json:"execute"`
		Arguments any    `json:"arguments,omitempty"`
		ID        int    `json:"id"`
	}{execute, args, q.nextID}
	if err := q.encoder.Encode(request); err != nil {
		return err
	}
	for {
		var reply struct {
			ID     int             `json:"id"`
			Return json.RawMessage `json:"return"`
			Error  *struct {
				Class       string `json:"class"`
				Description string `json:"desc"`
			} `json:"error"`
		}
		if err := q.decoder.Decode(&reply); err != nil {
			return err
		}
		if reply.ID != q.nextID {
			continue
		}
		if reply.Error != nil {
			return fmt.Errorf("QMP %s: %s", execute, reply.Error.Class)
		}
		if reply.Return == nil {
			return errors.New("QMP response has no result")
		}
		if out != nil {
			return json.Unmarshal(reply.Return, out)
		}
		return nil
	}
}

func (q *qmpClient) memoryCeiling() (int64, error) {
	var result struct {
		Base    int64 `json:"base-memory"`
		Plugged int64 `json:"plugged-memory"`
	}
	err := q.command("query-memory-size-summary", nil, &result)
	if err == nil && result.Base+result.Plugged <= 0 {
		err = errors.New("invalid VM memory ceiling")
	}
	return result.Base + result.Plugged, err
}

func (q *qmpClient) memory() (guestMemory, error) {
	var m guestMemory
	var interval int
	args := map[string]any{"path": balloonPath, "property": "guest-stats-polling-interval"}
	if err := q.command("qom-get", args, &interval); err != nil {
		return m, err
	}
	if interval != 5 {
		args["value"] = 5
		if err := q.command("qom-set", args, nil); err != nil {
			return m, err
		}
	}
	var balloon struct {
		Actual int64 `json:"actual"`
	}
	if err := q.command("query-balloon", nil, &balloon); err != nil {
		return m, err
	}
	m.ActualBytes = balloon.Actual
	ceiling, err := q.memoryCeiling()
	if err != nil {
		return m, err
	}
	m.CeilingBytes = ceiling
	var stats struct {
		Stats   map[string]int64 `json:"stats"`
		Updated int64            `json:"last-update"`
	}
	if err := q.command("qom-get", map[string]any{"path": balloonPath, "property": "guest-stats"}, &stats); err != nil {
		return m, err
	}
	m.UpdatedAt = stats.Updated
	m.AvailableBytes = -1
	m.OOMKills = -1
	m.CacheBytes = -1
	if value, ok := stats.Stats["stat-available-memory"]; ok {
		m.AvailableBytes = value
	}
	if value, ok := stats.Stats["stat-oom-kills"]; ok {
		m.OOMKills = value
	}
	if value, ok := stats.Stats["stat-disk-caches"]; ok {
		m.CacheBytes = value
	}
	return m, nil
}

func (q *qmpClient) setMemory(target int64) error {
	return q.command("balloon", map[string]any{"value": target}, nil)
}

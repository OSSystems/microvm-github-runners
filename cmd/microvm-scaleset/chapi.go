package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// chClient talks to one cloud-hypervisor's REST API (CH 52 OpenAPI) over its
// Unix socket.
type chClient struct {
	http *http.Client
}

func newCHClient(socket string) *chClient {
	return &chClient{http: &http.Client{
		Timeout: 5 * time.Second,
		// One connection per request: idle keep-alive connections would pile
		// up against the VMM's small connection limit.
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
	}}
}

func (c *chClient) do(ctx context.Context, method, endpoint string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost/api/v1/"+endpoint, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("cloud-hypervisor %s: %s: %s", endpoint, resp.Status, bytes.TrimSpace(raw))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// chVMInfo is the part of vm.info this controller reads.
type chVMInfo struct {
	State string `json:"state"`
	// MemoryActualSize is boot memory plus what virtio-mem has plugged.
	MemoryActualSize int64 `json:"memory_actual_size"`
}

func (c *chClient) info(ctx context.Context) (chVMInfo, error) {
	var info chVMInfo
	err := c.do(ctx, http.MethodGet, "vm.info", nil, &info)
	return info, err
}

// resizeMemory asks for desiredBytes of guest RAM; virtio-mem plugs or
// unplugs the difference above the boot size.
func (c *chClient) resizeMemory(ctx context.Context, desiredBytes int64) error {
	return c.do(ctx, http.MethodPut, "vm.resize", struct {
		DesiredRAM int64 `json:"desired_ram"`
	}{desiredBytes}, nil)
}

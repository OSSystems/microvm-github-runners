package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type apiCall struct {
	method, path string
	body         map[string]any
}

// fakeCH serves the cloud-hypervisor API on a Unix socket, records calls and
// counts the connections still open.
func fakeCH(t *testing.T, handler func(w http.ResponseWriter, call apiCall)) (string, *[]apiCall, *atomic.Int64) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "api.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := []apiCall{}
	open := &atomic.Int64{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := apiCall{method: r.Method, path: r.URL.Path}
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &call.body); err != nil {
				t.Errorf("%s body %q: %v", r.URL.Path, raw, err)
			}
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("%s without JSON content type", r.URL.Path)
			}
		}
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
		handler(w, call)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Add(-1)
		}
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return socket, &calls, open
}

func TestCHClientRequestShapes(t *testing.T) {
	socket, calls, _ := fakeCH(t, func(w http.ResponseWriter, call apiCall) {
		if call.path == "/api/v1/vm.info" {
			w.Write([]byte(`{"state":"Running","config":{"cpus":{"boot_vcpus":16,"max_vcpus":16},"memory":{"size":2147483648,"hotplug_size":1099511627776,"hotplug_method":"VirtioMem","shared":true},"payload":{"kernel":"/k"}},"memory_actual_size":3221225472}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	c := newCHClient(socket)
	ctx := context.Background()
	info, err := c.info(ctx)
	if err != nil || info.State != "Running" || info.MemoryActualSize != 3*gib {
		t.Fatalf("info = %+v, %v", info, err)
	}
	if err := c.resizeMemory(ctx, 3*gib); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 || (*calls)[0].method != "GET" || (*calls)[0].path != "/api/v1/vm.info" || (*calls)[1].method != "PUT" || (*calls)[1].path != "/api/v1/vm.resize" {
		t.Fatalf("calls = %+v", *calls)
	}
	if resize := (*calls)[1].body; len(resize) != 1 || resize["desired_ram"] != float64(3*gib) {
		t.Fatalf("vm.resize body = %v, want only desired_ram", resize)
	}
}

func TestCHClientReportsAPIErrors(t *testing.T) {
	socket, _, _ := fakeCH(t, func(w http.ResponseWriter, _ apiCall) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Error from API: The VM could not be resized"))
	})
	err := newCHClient(socket).resizeMemory(context.Background(), 4*gib)
	if err == nil || !strings.Contains(err.Error(), "could not be resized") {
		t.Fatalf("error = %v", err)
	}
}

// The memory loop calls vm.info on every VM every 2 s; no call may leave a
// connection open against the VMM's small connection limit.
func TestCHClientLeavesNoConnectionOpen(t *testing.T) {
	socket, _, open := fakeCH(t, func(w http.ResponseWriter, _ apiCall) {
		w.Write([]byte(`{"state":"Running","memory_actual_size":2147483648}`))
	})
	for range 12 {
		if _, err := (systemdVMs{}).memory(context.Background(), filepath.Dir(socket)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for open.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := open.Load(); n != 0 {
		t.Fatalf("%d API connections left open", n)
	}
}

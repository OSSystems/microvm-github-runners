package main

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// controlCall sends one request line to a control socket and decodes the
// reply.
func controlCall(t *testing.T, socket, request string) map[string]any {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(request + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if err := json.Unmarshal([]byte(line), &reply); err != nil {
		t.Fatalf("reply %q: %v", line, err)
	}
	return reply
}

func listenControl(t *testing.T, vm *chVM) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "vsock.sock_5001")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go serveControl(ln, vm)
	return socket
}

func TestControlChannelServesJITConfigOnce(t *testing.T) {
	vm := &chVM{jit: "ZW5jb2RlZA=="}
	socket := listenControl(t, vm)
	if got := controlCall(t, socket, `{"op":"jitconfig"}`); got["jitconfig"] != "ZW5jb2RlZA==" || got["error"] != nil {
		t.Fatalf("first jitconfig = %v", got)
	}
	got := controlCall(t, socket, `{"op":"jitconfig"}`)
	if got["jitconfig"] != nil || !strings.Contains(got["error"].(string), "already served") {
		t.Fatalf("second jitconfig = %v", got)
	}
}

func TestControlChannelStats(t *testing.T) {
	a, b := &chVM{}, &chVM{}
	socketA, socketB := listenControl(t, a), listenControl(t, b)
	// The VM is the one whose socket the guest came in on; a name in the
	// payload is ignored.
	got := controlCall(t, socketA, `{"op":"stats","name":"other","memAvailableKiB":1048576,"memTotalKiB":4194304,"psiSomeAvg10":7.5,"psiFullAvg10":0.25}`)
	if got["ok"] != true {
		t.Fatalf("stats reply = %v", got)
	}
	a.mu.Lock()
	s := a.mem.stats
	a.mu.Unlock()
	if s.MemAvailableKiB != 1<<20 || s.MemTotalKiB != 4<<20 || s.PSISomeAvg10 != 7.5 || s.PSIFullAvg10 != 0.25 || s.ReceivedAt.IsZero() {
		t.Fatalf("recorded stats = %+v", s)
	}
	if !b.mem.stats.ReceivedAt.IsZero() {
		t.Fatal("stats landed on the wrong VM")
	}
	for _, bad := range []string{
		`{"op":"stats","memAvailableKiB":1,"memTotalKiB":4}`,
		`{"op":"stats","memAvailableKiB":5,"memTotalKiB":4,"psiSomeAvg10":0,"psiFullAvg10":0}`,
		`{"op":"stats","memAvailableKiB":-1,"memTotalKiB":4,"psiSomeAvg10":0,"psiFullAvg10":0}`,
		`{"op":"stats","memAvailableKiB":1,"memTotalKiB":4,"psiSomeAvg10":101,"psiFullAvg10":0}`,
		`{"op":"reboot"}`,
		`not json`,
	} {
		if got := controlCall(t, socketB, bad); got["error"] == nil || got["ok"] != nil {
			t.Fatalf("%s accepted: %v", bad, got)
		}
	}
	if !b.mem.stats.ReceivedAt.IsZero() {
		t.Fatal("invalid stats were recorded")
	}
}

// A reply that never reached the guest leaves the JIT config for its retry.
func TestUndeliveredJITConfigIsServedAgain(t *testing.T) {
	vm := &chVM{jit: "ZW5jb2RlZA=="}
	server, client := net.Pipe()
	go func() {
		client.Write([]byte(`{"op":"jitconfig"}` + "\n"))
		client.Close()
	}()
	handleControl(server, vm)
	socket := listenControl(t, vm)
	if got := controlCall(t, socket, `{"op":"jitconfig"}`); got["jitconfig"] != "ZW5jb2RlZA==" {
		t.Fatalf("retry after a lost reply = %v", got)
	}
	if got := controlCall(t, socket, `{"op":"jitconfig"}`); got["error"] == nil {
		t.Fatalf("served twice: %v", got)
	}
}

func TestControlStatsCarryTheGivenTime(t *testing.T) {
	vm := &chVM{}
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	reply, _ := answerControl(bufio.NewReader(strings.NewReader(`{"op":"stats","memAvailableKiB":1,"memTotalKiB":4,"psiSomeAvg10":0,"psiFullAvg10":0}`+"\n")), vm, at)
	if !reply.OK || !vm.mem.stats.ReceivedAt.Equal(at) {
		t.Fatalf("reply %+v, receivedAt %v", reply, vm.mem.stats.ReceivedAt)
	}
}

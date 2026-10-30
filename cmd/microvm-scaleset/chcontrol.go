package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"
)

// controlVM is the VM a control socket belongs to. The socket identifies the
// VM: cloud-hypervisor forwards a guest's connection to port P to its own
// <vsock.sock>_P, so nothing in the payload is trusted to name it.
type controlVM interface {
	// takeJITConfig returns the runner's JIT config, to one request at a
	// time. delivered reports whether the reply reached the guest; only
	// then is the config used up.
	takeJITConfig() (jit string, delivered func(bool), err error)
	recordStats(guestStats)
}

type controlRequest struct {
	Op              string   `json:"op"`
	MemAvailableKiB *int64   `json:"memAvailableKiB"`
	MemTotalKiB     *int64   `json:"memTotalKiB"`
	PSISomeAvg10    *float64 `json:"psiSomeAvg10"`
	PSIFullAvg10    *float64 `json:"psiFullAvg10"`
}

type controlReply struct {
	JITConfig string `json:"jitconfig,omitempty"`
	OK        bool   `json:"ok,omitempty"`
	Error     string `json:"error,omitempty"`
}

// serveControl answers one newline-delimited JSON request per connection
// until ln closes.
func serveControl(ln net.Listener, vm controlVM) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		go handleControl(conn, vm)
	}
}

func handleControl(conn net.Conn, vm controlVM) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	reply, delivered := answerControl(bufio.NewReader(io.LimitReader(conn, 4096)), vm, time.Now())
	raw, _ := json.Marshal(reply)
	_, err := conn.Write(append(raw, '\n'))
	if delivered != nil {
		delivered(err == nil)
	}
}

func answerControl(r *bufio.Reader, vm controlVM, now time.Time) (controlReply, func(bool)) {
	line, err := r.ReadBytes('\n')
	if err != nil && (!errors.Is(err, io.EOF) || len(line) == 0) {
		return controlReply{Error: "expected one JSON line"}, nil
	}
	var req controlRequest
	if json.Unmarshal(line, &req) != nil {
		return controlReply{Error: "invalid JSON"}, nil
	}
	switch req.Op {
	case "jitconfig":
		jit, delivered, err := vm.takeJITConfig()
		if err != nil {
			return controlReply{Error: err.Error()}, nil
		}
		return controlReply{JITConfig: jit}, delivered
	case "stats":
		if req.MemAvailableKiB == nil || req.MemTotalKiB == nil || req.PSISomeAvg10 == nil || req.PSIFullAvg10 == nil {
			return controlReply{Error: "incomplete stats"}, nil
		}
		s := guestStats{MemAvailableKiB: *req.MemAvailableKiB, MemTotalKiB: *req.MemTotalKiB, PSISomeAvg10: *req.PSISomeAvg10, PSIFullAvg10: *req.PSIFullAvg10, ReceivedAt: now}
		if !s.valid() {
			return controlReply{Error: "invalid stats"}, nil
		}
		vm.recordStats(s)
		return controlReply{OK: true}, nil
	}
	return controlReply{Error: "unknown op"}, nil
}

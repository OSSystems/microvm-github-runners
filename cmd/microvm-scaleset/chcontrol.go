package main

import (
	"bufio"
	"net"
	"time"

	"github.com/OSSystems/vmkit-nix/control"
)

// controlVM is the VM a control socket belongs to.
type controlVM interface {
	// takeJITConfig returns the runner's JIT config, to one request at a
	// time. delivered reports whether the reply reached the guest; only
	// then is the config used up.
	takeJITConfig() (jit string, delivered func(bool), err error)
	recordStats(guestStats)
}

// controlReply adds the runner's JIT config to vmkit's reply.
type controlReply struct {
	control.Reply
	JITConfig string `json:"jitconfig,omitempty"`
}

func failed(r *control.Reply) controlReply { return controlReply{Reply: *r} }

// serveControl answers one newline-delimited JSON request per connection
// until ln closes.
func serveControl(ln net.Listener, vm controlVM) {
	control.Serve(ln, func(conn net.Conn) { handleControl(conn, vm) })
}

func handleControl(conn net.Conn, vm controlVM) {
	control.Answer(conn, func(r *bufio.Reader) (any, func(bool)) {
		return answerControl(r, vm, time.Now())
	})
}

func answerControl(r *bufio.Reader, vm controlVM, now time.Time) (controlReply, func(bool)) {
	req, bad := control.ReadRequest(r)
	if bad != nil {
		return failed(bad), nil
	}
	switch req.Op {
	case "jitconfig":
		jit, delivered, err := vm.takeJITConfig()
		if err != nil {
			return controlReply{Reply: control.Reply{Error: err.Error()}}, nil
		}
		return controlReply{JITConfig: jit}, delivered
	case "stats":
		s, bad := req.Stats(now)
		if bad != nil {
			return failed(bad), nil
		}
		vm.recordStats(s)
		return controlReply{Reply: control.Reply{OK: true}}, nil
	}
	return controlReply{Reply: control.Reply{Error: "unknown op"}}, nil
}

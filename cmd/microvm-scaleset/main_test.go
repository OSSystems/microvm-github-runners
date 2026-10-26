package main

import "testing"

func TestMemoryRoom(t *testing.T) {
	tests := []struct {
		name      string
		running   int
		available float64
		want      int
	}{
		{"available memory limits launch batch", 0, 39, 3},
		{"overcommit permits eighth VM", 7, 39, 3},
		{"assigned memory still caps concurrency", 10, 39, 0},
		{"host reserve is retained", 7, 17, 0},
		{"exactly one VM fits above reserve", 7, 18, 1},
		{"less than host reserve", 7, 4, 0},
	}
	p := &pool{cfg: config{vmMemGiB: 10, memReserveGiB: 8, memOvercommit: 1}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := p.memoryRoom(tt.running, map[string]float64{"MemTotal": 62.7, "MemAvailable": tt.available})
			if got != tt.want {
				t.Fatalf("room = %d, want %d (%s)", got, tt.want, reason)
			}
			if (got == 0) != (reason != "") {
				t.Fatalf("room = %d, reason = %q", got, reason)
			}
		})
	}
}

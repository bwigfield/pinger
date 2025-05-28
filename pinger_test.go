package pinger_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/bwigfield/pinger"
)

func TestPingLocalhost(t *testing.T) {
	skipIfNotCapable(t)

	results := make(chan pinger.Result, 10)
	p := pinger.NewPinger(context.Background(), results, 2*time.Second)

	go p.Start([]string{"127.0.0.1"}, time.Second)

	select {
	case res := <-results:
		if res.Error != nil {
			t.Errorf("Ping failed: %v", res.Error)
		} else {
			t.Logf("Got latency: %v", res.Latency)
		}
	case <-time.After(3 * time.Second):
		t.Error("Timeout waiting for ping result")
	}

	p.Stop()
}

func skipIfNotCapable(t *testing.T) {
	// Try to bind a raw socket to see if we're allowed
	_, err := net.ListenPacket("ip4:icmp", "127.0.0.1")
	if err != nil {
		t.Skipf("Skipping test: insufficient privileges for raw ICMP: %v", err)
	}
}

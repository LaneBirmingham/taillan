package portscan

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

// TestListeningFindsOwnSockets opens a TCP and a UDP port and checks
// the scan reports both as ours. It runs on every CI OS, so it catches
// a platform where the scan quietly doesn't work.
func TestListeningFindsOwnSockets(t *testing.T) {
	tl, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	port := uint16(tl.Addr().(*net.TCPAddr).Port)
	ul, err := net.ListenPacket("udp4", "127.0.0.1:"+strconv.Itoa(int(port)))
	if err != nil {
		t.Skipf("UDP port %d busy: %v", port, err)
	}
	defer ul.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	socks, err := Listening(ctx)
	if err != nil {
		t.Fatalf("Listening: %v", err)
	}
	for _, proto := range []string{"tcp", "udp"} {
		found := Find(socks, proto, port)
		if len(found) == 0 {
			t.Errorf("%s %d not found among %d sockets", proto, port, len(socks))
			continue
		}
		if !found[0].Self {
			t.Errorf("%s %d not marked as our own: %+v", proto, port, found[0])
		}
		if !found[0].Loopback() {
			t.Errorf("%s %d bound to 127.0.0.1 but Loopback() is false: %+v", proto, port, found[0])
		}
	}
}

func TestSpec(t *testing.T) {
	got := Spec([]Socket{
		{Proto: "udp", Port: 7777}, {Proto: "tcp", Port: 7777},
		{Proto: "tcp", Port: 27015}, {Proto: "udp", Port: 2456},
	})
	if want := "2456/udp, 7777/both, 27015/tcp"; got != want {
		t.Errorf("Spec = %q, want %q", got, want)
	}
}

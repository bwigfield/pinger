// pinger/pinger.go
package pinger

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type Result struct {
	Addr    string
	Latency time.Duration
	Error   error
	Time    time.Time
}

type pingMeta struct {
	Host string
	Sent time.Time
}

type Pinger struct {
	ctx     context.Context
	cancel  context.CancelFunc
	cm      *ConnManager
	results chan<- Result
	timeout time.Duration
	wg      sync.WaitGroup

	// sequence & outstanding tracking
	seqMu       sync.Mutex
	seq         int
	outMu       sync.Mutex
	outstanding map[int]pingMeta

	// offline detection
	offline   bool
	offlineMu sync.Mutex
}

func NewPinger(parent context.Context, results chan<- Result, timeout time.Duration) *Pinger {
	ctx, cancel := context.WithCancel(parent)
	cm := NewConnManager(ctx)
	p := &Pinger{
		ctx:         ctx,
		cancel:      cancel,
		cm:          cm,
		results:     results,
		timeout:     timeout,
		outstanding: make(map[int]pingMeta),
	}
	cm.OnReceive = p.handleReceive
	return p
}

// Start begins the scheduler, timeout checker, and connectivity watcher.
func (p *Pinger) Start(hosts []string, interval time.Duration) {
	p.wg.Add(1)
	go p.schedulerLoop(hosts, interval)

	p.wg.Add(1)
	go p.timeoutLoop()

	// Connectivity watcher: flip offline flag + clear outstanding on loss,
	// and clear offline on regain.

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-p.ctx.Done():
				return
			case <-ticker.C:
				conn, err := net.Dial("udp", "8.8.8.8:53")
				if err != nil {
					p.setOffline(true) // went offline → clear outstanding
				} else {
					conn.Close()
					p.setOffline(false) // back online
				}
			}
		}
	}()
}

func (p *Pinger) Stop() {
	p.cancel()
	p.wg.Wait()
	p.cm.CloseAll()
	close(p.results)
}

func (p *Pinger) nextSeq() int {
	p.seqMu.Lock()
	defer p.seqMu.Unlock()
	p.seq = (p.seq + 1) & 0xFFFF
	if p.seq == 0 {
		p.seq = 1
	}
	return p.seq
}

func (p *Pinger) register(seq int, host string, ts time.Time) {
	p.outMu.Lock()
	p.outstanding[seq] = pingMeta{Host: host, Sent: ts}
	p.outMu.Unlock()
}

// setOffline flips the offline flag; when going offline it also wipes any
// in-flight pings so they won’t timeout later.
func (p *Pinger) setOffline(v bool) {
	p.offlineMu.Lock()
	already := p.offline
	p.offline = v
	p.offlineMu.Unlock()

	if v && !already {
		// just went offline → clear all outstanding so no timeouts later
		p.outMu.Lock()
		p.outstanding = make(map[int]pingMeta)
		p.outMu.Unlock()
		log.Println("Pinger: went offline, cleared outstanding pings")
	}
}

func (p *Pinger) isOffline() bool {
	p.offlineMu.Lock()
	defer p.offlineMu.Unlock()
	return p.offline
}

func (p *Pinger) schedulerLoop(hosts []string, interval time.Duration) {
	defer p.wg.Done()

	var targets []struct {
		host, dstStr string
		dst          *net.IPAddr
		typ          icmp.Type
	}
	for _, host := range hosts {
		netw := "ip4"
		if strings.Contains(host, ":") {
			netw = "ip6"
		}
		dst, err := net.ResolveIPAddr(netw, host)
		if err != nil {
			p.results <- Result{Addr: host, Error: fmt.Errorf("resolve %s: %w", host, err)}
			continue
		}
		var t icmp.Type
		if dst.IP.To4() != nil {
			t = ipv4.ICMPTypeEcho
		} else {
			t = ipv6.ICMPTypeEchoRequest
		}
		targets = append(targets, struct {
			host, dstStr string
			dst          *net.IPAddr
			typ          icmp.Type
		}{host, dst.IP.String(), dst, t})
	}
	if len(targets) == 0 {
		log.Println("No valid targets to ping")
		return
	}

	step := interval / time.Duration(len(targets))
	ticker := time.NewTicker(step)
	defer ticker.Stop()
	i := 0

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			if p.isOffline() {
				continue
			}
			t := targets[i]
			p.sendPing(t.host, t.dst, t.dstStr, t.typ)
			i = (i + 1) % len(targets)
		}
	}
}

func (p *Pinger) sendPing(host string, dst *net.IPAddr, dstStr string, msgType icmp.Type) {
	// 1) if we’re already offline, skip immediately
	if p.isOffline() {
		return
	}

	conn, err := p.cm.GetConnForDest(dstStr)
	if err != nil {
		p.results <- Result{Addr: host, Error: fmt.Errorf("get socket: %w", err)}
		return
	}

	seq := p.nextSeq()
	ts := time.Now()
	p.register(seq, dstStr, ts)

	// prepare and marshal ICMP echo…
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, uint64(ts.UnixNano()))
	echo := &icmp.Echo{ID: 1, Seq: seq, Data: data}
	msg := icmp.Message{Type: msgType, Code: 0, Body: echo}

	msgBuf, err := msg.Marshal(nil)
	if err != nil {
		p.results <- Result{Addr: host, Error: fmt.Errorf("marshal icmp: %w", err)}
		return
	}

	if _, err := conn.WriteTo(msgBuf, dst); err != nil {
		// on network-down errors, flip offline *immediately* and bail
		if strings.Contains(err.Error(), "network is unreachable") {
			p.setOffline(true)
			return
		}
		// otherwise handle as before
		p.results <- Result{Addr: host, Error: fmt.Errorf("write icmp: %w", err)}
		log.Println("sendPing: resetting socket due to error:", err)
		p.cm.ResetConn(dstStr)
		time.Sleep(100 * time.Millisecond)
	}
}

func (p *Pinger) handleReceive(msg *icmp.Message, addr net.Addr, recvTime time.Time) {
	echo, ok := msg.Body.(*icmp.Echo)
	if !ok {
		return
	}

	seq := echo.Seq

	p.outMu.Lock()
	meta, seen := p.outstanding[seq]
	if seen {
		delete(p.outstanding, seq)
	}
	p.outMu.Unlock()

	if !seen {
		return
	}

	p.results <- Result{
		Addr:    meta.Host,
		Latency: recvTime.Sub(meta.Sent),
		Time:    meta.Sent,
	}
}

func (p *Pinger) timeoutLoop() {
	defer p.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case now := <-ticker.C:
			p.outMu.Lock()
			for seq, meta := range p.outstanding {
				if now.Sub(meta.Sent) > p.timeout {
					delete(p.outstanding, seq)
					p.results <- Result{Addr: meta.Host, Error: fmt.Errorf("timeout after %s", p.timeout)}
				}
			}
			p.outMu.Unlock()
		}
	}
}

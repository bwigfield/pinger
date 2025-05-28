package pinger

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type ConnManager struct {
	ctx              context.Context
	cancel           context.CancelFunc
	mu               sync.Mutex
	connMap          map[string]*icmp.PacketConn
	localForRemote   map[netip.Addr]string // remote IP → local IP
	subnetToLocal    map[string]string
	lastResponse     map[string]time.Time
	recoveryCooldown map[string]time.Time
	recovering       map[string]bool

	OnReceive func(msg *icmp.Message, addr net.Addr, recvTime time.Time)
}

func NewConnManager(parentCtx context.Context) *ConnManager {
	ctx, cancel := context.WithCancel(parentCtx)
	cm := &ConnManager{
		ctx:              ctx,
		cancel:           cancel,
		connMap:          make(map[string]*icmp.PacketConn),
		localForRemote:   make(map[netip.Addr]string),
		subnetToLocal:    make(map[string]string),
		lastResponse:     make(map[string]time.Time),
		recoveryCooldown: make(map[string]time.Time),
		recovering:       make(map[string]bool),
		OnReceive:        func(_ *icmp.Message, _ net.Addr, _ time.Time) {},
	}
	if v6conn, err := icmp.ListenPacket("ip6:ipv6-icmp", "::"); err == nil {
		cm.connMap["::"] = v6conn
		go cm.recvLoop(v6conn)
	} else {
		log.Printf("warning: unable to bind IPv6 ICMP: %v\n", err)
	}
	return cm
}

func (cm *ConnManager) CloseAll() {
	cm.cancel()
	cm.mu.Lock()
	defer cm.mu.Unlock()
	for _, conn := range cm.connMap {
		conn.Close()
	}
	cm.connMap = make(map[string]*icmp.PacketConn)
	cm.subnetToLocal = make(map[string]string)
}
func (cm *ConnManager) GetConnForDest(destIPStr string) (*icmp.PacketConn, error) {
	ipAddr, err := netip.ParseAddr(destIPStr)
	if err != nil {
		return nil, fmt.Errorf("invalid IP: %w", err)
	}

	if ipAddr.Is6() {
		cm.mu.Lock()
		v6c, ok := cm.connMap["::"]
		cm.mu.Unlock()
		if ok {
			return v6c, nil
		}
		v6conn, err := icmp.ListenPacket("ip6:ipv6-icmp", "::")
		if err != nil {
			log.Printf("GetConnForDest: failed to bind IPv6: %v", err)
			return nil, fmt.Errorf("no IPv6 socket available: %w", err)
		}
		cm.mu.Lock()
		cm.connMap["::"] = v6conn
		cm.mu.Unlock()
		go cm.recvLoop(v6conn)
		log.Printf("GetConnForDest: re-created IPv6 wildcard socket")
		return v6conn, nil
	}

	cm.mu.Lock()
	if local, ok := cm.localForRemote[ipAddr]; ok {
		conn := cm.connMap[local]
		cm.mu.Unlock()
		return conn, nil
	}
	cm.mu.Unlock()

	dummy, err := net.Dial("udp", net.JoinHostPort(destIPStr, "80"))
	if err != nil {
		return nil, fmt.Errorf("dial udp to %s: %w", destIPStr, err)
	}
	localIPStr := dummy.LocalAddr().(*net.UDPAddr).IP.String()
	dummy.Close()

	cm.mu.Lock()
	cm.localForRemote[ipAddr] = localIPStr
	if _, ok := cm.connMap[localIPStr]; !ok {
		conn, err := icmp.ListenPacket("ip4:icmp", localIPStr)
		if err != nil {
			cm.mu.Unlock()
			return nil, fmt.Errorf("listen icmp on %s: %w", localIPStr, err)
		}
		cm.connMap[localIPStr] = conn
		log.Printf("GetConnForDest: created IPv4 socket on %s for dest %s", localIPStr, destIPStr)
		key := localIPStr // whatever you use to identify this socket
		goWithRecovery(cm.ctx, "recvLoop-"+key, func() {
			cm.recvLoop(conn)
		})
	}
	conn := cm.connMap[localIPStr]
	cm.mu.Unlock()
	return conn, nil
}

func (cm *ConnManager) recvLoop(conn *icmp.PacketConn) {
	localKey := conn.LocalAddr().String()
	proto := ipv4.ICMPTypeEchoReply.Protocol()
	if ipAddr, ok := conn.LocalAddr().(*net.IPAddr); ok && ipAddr.IP.To4() == nil {
		proto = ipv6.ICMPTypeEchoReply.Protocol()
	}

	buf := make([]byte, 1500)
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()

	pingInterval := 5 * time.Second
	resetTimeout := 2 * pingInterval

	for {
		select {
		case <-cm.ctx.Done():
			return
		case <-tick.C:
			cm.mu.Lock()
			lastResp, ok := cm.lastResponse[localKey]
			lastRecovery := cm.recoveryCooldown[localKey]
			now := time.Now()
			if !ok || now.Sub(lastResp) > resetTimeout {
				if now.Sub(lastRecovery) < 2*time.Second {
					cm.mu.Unlock()
					continue
				}
				cm.recoveryCooldown[localKey] = now
				cm.mu.Unlock()
				log.Printf("recvLoop timeout: no ICMP response on %s in %v — triggering socket recovery", localKey, resetTimeout)
				if newConn := cm.recoverSocket(localKey); newConn != nil {
					conn = newConn
					continue
				}
				return
			}
			cm.mu.Unlock()
		default:
		}

		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if strings.Contains(err.Error(), "use of closed network connection") {
				log.Printf("recvLoop detected closed socket on %s — attempting to recover", localKey)
				if newConn := cm.recoverSocket(localKey); newConn != nil {
					conn = newConn
					continue
				}
				return
			}
			log.Printf("recvLoop ReadFrom error on %s: %v", localKey, err)
			return
		}
		cm.mu.Lock()
		cm.lastResponse[localKey] = time.Now()
		cm.mu.Unlock()

		msg, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil {
			log.Printf("recvLoop ParseMessage error on %s: %v", localKey, err)
			continue
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("panic in OnReceive handler for %s: %#v", localKey, r)
				}
			}()
			cm.OnReceive(msg, addr, time.Now())
		}()
	}
}

func (cm *ConnManager) recoverSocket(localKey string) *icmp.PacketConn {
	cm.mu.Lock()
	if cm.recovering[localKey] {
		cm.mu.Unlock()
		log.Printf("recoverSocket: already recovering socket for %s", localKey)
		return nil
	}
	cm.recovering[localKey] = true
	cm.mu.Unlock()

	defer func() {
		cm.mu.Lock()
		delete(cm.recovering, localKey)
		cm.mu.Unlock()
	}()

	// Tear down any existing socket for this key.
	cm.mu.Lock()
	if old, ok := cm.connMap[localKey]; ok {
		old.Close()
		delete(cm.connMap, localKey)
	}
	cm.mu.Unlock()

	var (
		newConn *icmp.PacketConn
		err     error
		network string
	)
	if strings.Contains(localKey, ":") {
		network = "ip6:ipv6-icmp"
	} else {
		network = "ip4:icmp"
	}

	// Retry loop with 1s backoff until success or shutdown
	for {
		newConn, err = icmp.ListenPacket(network, localKey)
		if err == nil {
			break
		}
		log.Printf("recoverSocket: failed to bind %s on %s: %v; retrying in 1s", network, localKey, err)

		select {
		case <-time.After(time.Second):
			// try again
		case <-cm.ctx.Done():
			log.Printf("recoverSocket: aborting recovery for %s due to shutdown", localKey)
			return nil
		}
	}

	// Store and log success
	cm.mu.Lock()
	cm.connMap[localKey] = newConn
	cm.mu.Unlock()
	log.Printf("recoverSocket: rebound socket %s on %s", network, localKey)

	return newConn
}

func (cm *ConnManager) ResetConn(destIP string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Figure out which local socketsent this ping
	var key string
	ipAddr, err := netip.ParseAddr(destIP)
	if err != nil {
		return
	}
	if ipAddr.Is6() {
		key = "::"
	} else {
		localIP, ok := cm.localForRemote[ipAddr]
		if !ok {
			return
		}
		key = localIP
	}

	// Tear down that socket
	if old, ok := cm.connMap[key]; ok {
		old.Close()
		delete(cm.connMap, key)
		log.Printf("ResetConn: closed socket for key=%s", key)
	}

	// Spin up a fresh one
	go cm.recoverSocket(key)
}

func goWithRecovery(ctx context.Context, name string, fn func()) {
	go func() {
		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("%s: panic recovered: %v\n%s", name, r, debug.Stack())
					}
				}()
				fn()
			}()
			select {
			case <-ctx.Done():
				log.Printf("%s: context cancelled; stopping restarts", name)
				return
			default:
				log.Printf("%s: restarting after exit/panic", name)
			}
		}
	}()
}

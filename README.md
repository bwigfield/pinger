# Pinger

**Pinger** is a high-performance, concurrent ICMP ping utility written in Go. It handles IPv4 and IPv6, intelligently manages raw socket bindings based on source/destination IP pairs, and includes robust offline detection and socket recovery logic.

### Features

- ✅ Concurrent ICMP pinging with proper sequence tracking
- ✅ Handles both IPv4 and IPv6 seamlessly
- ✅ Automatic socket recovery on failure
- ✅ Offline detection with fast fallback behavior
- ✅ Clean shutdown and timeout handling
- ✅ Zero external dependencies beyond `golang.org/x/net`

### Why this exists

This isn't a toy ping loop. It's designed for integration into real systems or anything that benefits from precise ICMP measurements without external binaries or root hacks.

### Usage

Create a `Pinger`, pass it a context, a results channel, and a timeout. Then call `Start()` with your list of hosts and desired interval.

```go
results := make(chan pinger.Result, 100)
p := pinger.NewPinger(context.Background(), results, 3*time.Second)
p.Start([]string{"8.8.8.8", "1.1.1.1"}, 1*time.Second)

go func() {
    for res := range results {
        if res.Error != nil {
            fmt.Printf("Ping error for %s: %v\n", res.Addr, res.Error)
        } else {
            fmt.Printf("Ping to %s: %v\n", res.Addr, res.Latency)
        }
    }
}()

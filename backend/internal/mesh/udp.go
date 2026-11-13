package mesh

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/log"
)

const (
	// udpIdle ends a flow when no datagram passed either way for this long.
	udpIdle = 2 * time.Minute
	// udpRetry is how long datagrams of a flow whose stream could not open
	// are dropped before the next try.
	udpRetry = 10 * time.Second
	// udpQueue datagrams wait for a flow's stream; more are dropped, as a
	// congested link would.
	udpQueue = 64
)

// udpRelay carries a session's captured UDP flows to the egress gateway: each
// flow, a source and destination pair, gets its own connect-udp stream, and
// what comes back is sent to the source as if from the destination.
type udpRelay struct {
	session string
	log     *log.Logger
	// open makes a connect-udp stream to dst.
	open func(ctx context.Context, dst netip.AddrPort) (io.ReadWriteCloser, error)
	// replyAs returns a socket that sends with dst as its source address.
	replyAs func(dst netip.AddrPort) (net.PacketConn, error)
	idle    time.Duration
	retry   time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	flows map[[2]netip.AddrPort]*udpFlow
}

type udpFlow struct {
	src, dst netip.AddrPort
	queue    chan []byte
	last     atomic.Int64 // unix nanoseconds of the last datagram either way

	mu       sync.Mutex
	failedAt time.Time // the stream did not open; drop until udpRetry passed
	closed   bool
	stream   io.ReadWriteCloser
	reply    net.PacketConn
}

func newUDPRelay(session string, logger *log.Logger, idle, retry time.Duration, open func(context.Context, netip.AddrPort) (io.ReadWriteCloser, error), replyAs func(netip.AddrPort) (net.PacketConn, error)) *udpRelay {
	ctx, cancel := context.WithCancel(context.Background())
	r := &udpRelay{session: session, log: logger, open: open, replyAs: replyAs, idle: idle, retry: retry, ctx: ctx, cancel: cancel, flows: map[[2]netip.AddrPort]*udpFlow{}}
	go r.expire()
	return r
}

// packet takes a datagram the session sent from src to dst.
func (r *udpRelay) packet(src, dst netip.AddrPort, payload []byte) {
	key := [2]netip.AddrPort{src, dst}
	r.mu.Lock()
	if r.ctx.Err() != nil {
		r.mu.Unlock()
		return
	}
	f, ok := r.flows[key]
	if !ok {
		f = &udpFlow{src: src, dst: dst, queue: make(chan []byte, udpQueue)}
		r.flows[key] = f
		go r.run(f)
	}
	r.mu.Unlock()
	f.touch()
	f.mu.Lock()
	if !f.closed {
		select {
		case f.queue <- payload:
		default:
		}
	}
	f.mu.Unlock()
}

func (f *udpFlow) touch() { f.last.Store(time.Now().UnixNano()) }

// run opens the flow's stream and pumps it until the stream or the flow ends.
func (r *udpRelay) run(f *udpFlow) {
	stream, err := r.open(r.ctx, f.dst)
	var reply net.PacketConn
	if err == nil {
		if reply, err = r.replyAs(f.dst); err != nil {
			stream.Close()
		}
	}
	if err != nil {
		r.log.Warn("udp flow", "session", r.session, "destination", f.dst, "error", err)
		f.mu.Lock()
		f.failedAt = time.Now()
		f.mu.Unlock()
		r.drain(f)
		return
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		stream.Close()
		reply.Close()
		return
	}
	f.stream, f.reply = stream, reply
	f.mu.Unlock()
	go func() {
		defer r.end(f)
		br := bufio.NewReader(stream)
		to := net.UDPAddrFromAddrPort(f.src)
		for {
			p, err := readDatagram(br)
			if err != nil {
				return
			}
			f.touch()
			_, _ = reply.WriteTo(p, to)
		}
	}()
	for {
		select {
		case p, ok := <-f.queue:
			if !ok {
				return
			}
			if writeDatagram(stream, p) != nil {
				r.end(f)
				return
			}
		case <-r.ctx.Done():
			return
		}
	}
}

// drain drops what a failed flow is sent until it expires.
func (r *udpRelay) drain(f *udpFlow) {
	for range f.queue {
	}
}

// end closes a flow and forgets it, so the next datagram opens a new one.
func (r *udpRelay) end(f *udpFlow) {
	r.mu.Lock()
	if r.flows[[2]netip.AddrPort{f.src, f.dst}] == f {
		delete(r.flows, [2]netip.AddrPort{f.src, f.dst})
	}
	r.mu.Unlock()
	f.close()
}

func (f *udpFlow) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	close(f.queue)
	if f.stream != nil {
		f.stream.Close()
		f.reply.Close()
	}
}

// expire ends flows that went idle and failed flows once their retry is due.
func (r *udpRelay) expire() {
	t := time.NewTicker(min(r.idle, r.retry) / 4)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
		}
		var due []*udpFlow
		r.mu.Lock()
		for key, f := range r.flows {
			f.mu.Lock()
			failedAt := f.failedAt
			f.mu.Unlock()
			idle := time.Since(time.Unix(0, f.last.Load())) > r.idle
			if retry := !failedAt.IsZero() && time.Since(failedAt) > r.retry; idle || retry {
				delete(r.flows, key)
				due = append(due, f)
			}
		}
		r.mu.Unlock()
		for _, f := range due {
			f.close()
		}
	}
}

func (r *udpRelay) close() {
	r.mu.Lock()
	r.cancel()
	flows := r.flows
	r.flows = map[[2]netip.AddrPort]*udpFlow{}
	r.mu.Unlock()
	for _, f := range flows {
		f.close()
	}
}

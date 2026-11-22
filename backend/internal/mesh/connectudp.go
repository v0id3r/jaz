package mesh

// A client for UDP proxying over HTTP (RFC 9298), the egress gateway's
// connect-udp: an HTTP/2 extended CONNECT (RFC 8441) whose stream carries UDP
// payloads as DATAGRAM capsules (RFC 9297) with context ID 0. The agent
// broker has the same code in its egress package.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

const (
	connectUDP = "connect-udp"

	capsuleDatagram = 0x00
	// maxCapsule bounds a capsule: a UDP payload with its context ID and some slack.
	maxCapsule = 1 << 16
)

// udpPath is the default URI template's path to dst; the colons of an IPv6
// address are percent-encoded.
func udpPath(dst netip.AddrPort) string {
	return "/.well-known/masque/udp/" + strings.ReplaceAll(dst.Addr().String(), ":", "%3A") + "/" + strconv.Itoa(int(dst.Port())) + "/"
}

// writeDatagram writes payload as a DATAGRAM capsule with context ID 0.
func writeDatagram(w io.Writer, payload []byte) error {
	b := make([]byte, 0, len(payload)+10)
	b = appendVarint(b, capsuleDatagram)
	b = appendVarint(b, uint64(len(payload)+1))
	b = append(b, 0) // context ID 0
	b = append(b, payload...)
	_, err := w.Write(b)
	return err
}

// readDatagram returns the next UDP payload on r, skipping capsules of other types and
// datagrams with other context IDs, as RFC 9297 asks of unknown ones.
func readDatagram(r *bufio.Reader) ([]byte, error) {
	for {
		typ, err := readVarint(r)
		if err != nil {
			return nil, err
		}
		n, err := readVarint(r)
		if err != nil {
			return nil, noEOF(err)
		}
		if n > maxCapsule {
			return nil, fmt.Errorf("mesh: capsule of %d bytes", n)
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, noEOF(err)
		}
		if typ != capsuleDatagram {
			continue
		}
		if ctx, n := varintAt(b); n > 0 && ctx == 0 {
			return b[n:], nil
		}
	}
}

// varintAt decodes the varint at the start of b and its length, 0 if b is too short.
func varintAt(b []byte) (uint64, int) {
	if len(b) == 0 {
		return 0, 0
	}
	n := 1 << (b[0] >> 6)
	if len(b) < n {
		return 0, 0
	}
	v := uint64(b[0] & 0x3f)
	for _, c := range b[1:n] {
		v = v<<8 | uint64(c)
	}
	return v, n
}

func noEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// appendVarint appends v as a QUIC variable-length integer (RFC 9000 §16).
func appendVarint(b []byte, v uint64) []byte {
	switch {
	case v < 1<<6:
		return append(b, byte(v))
	case v < 1<<14:
		return append(b, byte(v>>8)|0x40, byte(v))
	case v < 1<<30:
		return append(b, byte(v>>24)|0x80, byte(v>>16), byte(v>>8), byte(v))
	default:
		return append(b, byte(v>>56)|0xc0, byte(v>>48), byte(v>>40), byte(v>>32), byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
}

func readVarint(r io.ByteReader) (uint64, error) {
	first, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	v := uint64(first & 0x3f)
	for range 1<<(first>>6) - 1 {
		b, err := r.ReadByte()
		if err != nil {
			return 0, noEOF(err)
		}
		v = v<<8 | uint64(b)
	}
	return v, nil
}

// openUDP opens a connect-udp stream to dst on conn, a fresh connection that has
// negotiated HTTP/2, and returns the stream: its bytes are capsules (see writeDatagram and
// readDatagram). The stream owns conn and closes it.
//
// It speaks HTTP/2 itself because Go 1.27's net/http, which x/net's transport now wraps,
// rejects the :protocol pseudo-header an extended CONNECT (RFC 8441) needs.
func openUDP(ctx context.Context, conn net.Conn, authority string, dst netip.AddrPort) (io.ReadWriteCloser, error) {
	s := &udpStream{conn: conn, fr: http2.NewFramer(conn, conn), sendWin: initialWindow, connWin: initialWindow, data: make(chan []byte, 64), done: make(chan struct{})}
	s.cond = sync.NewCond(&s.mu)
	s.fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := s.open(authority, udpPath(dst)); err != nil {
		conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	go s.readLoop()
	return s, nil
}

const (
	udpStreamID   = 1
	initialWindow = 65535
	// recvWindow is what the stream and the connection may have in flight towards us.
	recvWindow = 1 << 20
)

type udpStream struct {
	conn net.Conn
	fr   *http2.Framer
	wmu  sync.Mutex // guards fr's writes

	mu       sync.Mutex
	cond     *sync.Cond
	sendWin  int64 // stream send window
	connWin  int64
	maxFrame int
	err      error // set once the stream is over

	data chan []byte
	rest []byte
	done chan struct{}
	once sync.Once
}

// open sends the preface and settings, waits for the server's settings to allow extended
// CONNECT, sends the request and waits for its response.
func (s *udpStream) open(authority, path string) error {
	s.maxFrame = 16384
	if _, err := io.WriteString(s.conn, http2.ClientPreface); err != nil {
		return err
	}
	err := s.write(func() error {
		if err := s.fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: recvWindow}); err != nil {
			return err
		}
		return s.fr.WriteWindowUpdate(0, recvWindow-initialWindow)
	})
	if err != nil {
		return err
	}
	for sent := false; ; {
		f, err := s.fr.ReadFrame()
		if err != nil {
			return err
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if f.IsAck() {
				continue
			}
			if err := s.settings(f); err != nil {
				return err
			}
			if v, ok := f.Value(http2.SettingEnableConnectProtocol); !sent && (!ok || v != 1) {
				return errors.New("mesh: server does not allow extended CONNECT")
			}
			if !sent {
				if err := s.request(authority, path); err != nil {
					return err
				}
				sent = true
			}
		case *http2.MetaHeadersFrame:
			if f.StreamID != udpStreamID {
				continue
			}
			if st := f.PseudoValue("status"); st != "200" {
				return fmt.Errorf("mesh: connect-udp: status %s", st)
			}
			if f.StreamEnded() {
				return io.ErrUnexpectedEOF
			}
			return nil
		default:
			if err := s.handle(f); err != nil {
				return err
			}
		}
	}
}

func (s *udpStream) request(authority, path string) error {
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{
		{":method", "CONNECT"}, {":protocol", connectUDP}, {":scheme", "https"},
		{":authority", authority}, {":path", path}, {"capsule-protocol", "?1"},
	} {
		enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	return s.write(func() error {
		return s.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: udpStreamID, BlockFragment: hb.Bytes(), EndHeaders: true})
	})
}

// settings acknowledges a SETTINGS frame and applies what concerns the sender.
func (s *udpStream) settings(f *http2.SettingsFrame) error {
	s.mu.Lock()
	err := f.ForeachSetting(func(st http2.Setting) error {
		switch st.ID {
		case http2.SettingInitialWindowSize:
			s.sendWin += int64(st.Val) - initialWindow
		case http2.SettingMaxFrameSize:
			s.maxFrame = int(st.Val)
		}
		return nil
	})
	s.cond.Broadcast()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.write(s.fr.WriteSettingsAck)
}

// handle deals with a frame other than the response headers.
func (s *udpStream) handle(f http2.Frame) error {
	switch f := f.(type) {
	case *http2.SettingsFrame:
		if !f.IsAck() {
			return s.settings(f)
		}
	case *http2.PingFrame:
		if !f.IsAck() {
			return s.write(func() error { return s.fr.WritePing(true, f.Data) })
		}
	case *http2.WindowUpdateFrame:
		s.mu.Lock()
		if f.StreamID == 0 {
			s.connWin += int64(f.Increment)
		} else if f.StreamID == udpStreamID {
			s.sendWin += int64(f.Increment)
		}
		s.cond.Broadcast()
		s.mu.Unlock()
	case *http2.DataFrame:
		if f.StreamID != udpStreamID {
			return nil
		}
		if b := f.Data(); len(b) > 0 {
			select {
			case s.data <- bytes.Clone(b):
			case <-s.done:
				return net.ErrClosed
			}
		}
		// Padding counts against the windows but never reaches the reader.
		if pad := int(f.Length) - len(f.Data()); pad > 0 {
			s.credit(pad)
		}
		if f.StreamEnded() {
			return io.EOF
		}
	case *http2.RSTStreamFrame:
		if f.StreamID == udpStreamID {
			return fmt.Errorf("mesh: connect-udp reset: %v", f.ErrCode)
		}
	case *http2.GoAwayFrame:
		if f.LastStreamID < udpStreamID || f.ErrCode != http2.ErrCodeNo {
			return fmt.Errorf("mesh: connect-udp goaway: %v", f.ErrCode)
		}
	}
	return nil
}

func (s *udpStream) readLoop() {
	var err error
	for err == nil {
		var f http2.Frame
		if f, err = s.fr.ReadFrame(); err == nil {
			err = s.handle(f)
		}
	}
	s.fail(err)
	close(s.data)
}

// fail ends the stream with err unless it has ended already.
func (s *udpStream) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *udpStream) write(f func() error) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return f()
}

// credit returns n received bytes to the server's windows once they are consumed.
func (s *udpStream) credit(n int) {
	s.write(func() error {
		if err := s.fr.WriteWindowUpdate(0, uint32(n)); err != nil {
			return err
		}
		return s.fr.WriteWindowUpdate(udpStreamID, uint32(n))
	})
}

func (s *udpStream) Read(p []byte) (int, error) {
	if len(s.rest) == 0 {
		b, ok := <-s.data
		if !ok {
			s.mu.Lock()
			err := s.err
			s.mu.Unlock()
			if err == nil || errors.Is(err, net.ErrClosed) {
				err = io.EOF
			}
			return 0, err
		}
		s.rest = b
	}
	n := copy(p, s.rest)
	s.rest = s.rest[n:]
	s.credit(n)
	return n, nil
}

// Write sends p as DATA frames as the server's windows allow.
func (s *udpStream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		s.mu.Lock()
		for s.err == nil && (s.sendWin <= 0 || s.connWin <= 0) {
			s.cond.Wait()
		}
		if s.err != nil {
			err := s.err
			s.mu.Unlock()
			return written, err
		}
		n := int(min(int64(len(p)), s.sendWin, s.connWin, int64(s.maxFrame)))
		s.sendWin -= int64(n)
		s.connWin -= int64(n)
		s.mu.Unlock()
		if err := s.write(func() error { return s.fr.WriteData(udpStreamID, false, p[:n]) }); err != nil {
			s.fail(err)
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// Close ends the stream and its connection.
func (s *udpStream) Close() error {
	s.once.Do(func() {
		close(s.done)
		s.fail(net.ErrClosed)
		s.write(func() error { return s.fr.WriteRSTStream(udpStreamID, http2.ErrCodeCancel) })
		s.conn.Close()
	})
	return nil
}

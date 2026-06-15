package b2bua

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
)

// ErrPortsExhausted is returned by PortAllocator.Acquire when the range is full.
var ErrPortsExhausted = errors.New("RTP port range exhausted")

// PortPair holds an even RTP port and its odd RTCP companion.
type PortPair struct {
	RTP  int
	RTCP int
}

// PortAllocator manages a contiguous range of UDP port pairs (RTP even, RTCP = RTP+1).
type PortAllocator struct {
	mu   sync.Mutex
	min  int
	max  int
	free []PortPair
	next int
}

func newPortAllocator(min, max int) *PortAllocator {
	return &PortAllocator{min: min, max: max, next: min}
}

// Acquire returns the next free even/odd port pair. Returns ErrPortsExhausted when full.
func (p *PortAllocator) Acquire() (PortPair, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.free) > 0 {
		pair := p.free[len(p.free)-1]
		p.free = p.free[:len(p.free)-1]
		return pair, nil
	}
	if p.next+1 > p.max {
		return PortPair{}, ErrPortsExhausted
	}
	pair := PortPair{RTP: p.next, RTCP: p.next + 1}
	p.next += 2
	return pair, nil
}

// Release returns a port pair to the free pool.
func (p *PortAllocator) Release(pair PortPair) {
	p.mu.Lock()
	p.free = append(p.free, pair)
	p.mu.Unlock()
}

// AnchorSide is one anchored RTP/RTCP endpoint (endpoint-facing or PBX-facing).
// remoteRTP and remoteRTCP are swapped atomically so the relay goroutine reads a
// live value on every packet without taking a lock.
type AnchorSide struct {
	rtpConn      *net.UDPConn
	rtcpConn     *net.UDPConn
	remoteRTP    atomic.Pointer[net.UDPAddr]
	remoteRTCP   atomic.Pointer[net.UDPAddr]
	localRTPPort int
	pair         PortPair
}

// setRemote atomically updates the relay destinations. Pass nil to drop traffic
// in that direction (e.g. hold with 0.0.0.0 or before first negotiation).
func (s *AnchorSide) setRemote(rtp, rtcp *net.UDPAddr) {
	s.remoteRTP.Store(rtp)
	s.remoteRTCP.Store(rtcp)
}

func (s *AnchorSide) loadRemoteRTP() *net.UDPAddr  { return s.remoteRTP.Load() }
func (s *AnchorSide) loadRemoteRTCP() *net.UDPAddr { return s.remoteRTCP.Load() }

// newAnchorSide binds RTP and RTCP UDP sockets on mediaHost using the given pair.
func newAnchorSide(mediaHost string, pair PortPair) (*AnchorSide, error) {
	rtpAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", mediaHost, pair.RTP))
	if err != nil {
		return nil, fmt.Errorf("resolve RTP addr: %w", err)
	}
	rtpConn, err := net.ListenUDP("udp", rtpAddr)
	if err != nil {
		return nil, fmt.Errorf("bind RTP %s:%d: %w", mediaHost, pair.RTP, err)
	}

	rtcpAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", mediaHost, pair.RTCP))
	if err != nil {
		rtpConn.Close()
		return nil, fmt.Errorf("resolve RTCP addr: %w", err)
	}
	rtcpConn, err := net.ListenUDP("udp", rtcpAddr)
	if err != nil {
		rtpConn.Close()
		return nil, fmt.Errorf("bind RTCP %s:%d: %w", mediaHost, pair.RTCP, err)
	}

	return &AnchorSide{
		rtpConn:      rtpConn,
		rtcpConn:     rtcpConn,
		localRTPPort: pair.RTP,
		pair:         pair,
	}, nil
}

func (s *AnchorSide) close() {
	s.rtpConn.Close()
	s.rtcpConn.Close()
}

// Security reports that an AnchorSide is a plain (unencrypted) RTP leg. AnchorSide
// thus satisfies MediaLeg, letting the engine treat plain and secured legs uniformly.
func (s *AnchorSide) Security() MediaSecurity { return SecurityPlainRTP }

// ReadRTP reads one RTP packet from the leg's RTP socket. For a plain leg the wire
// bytes are already plaintext, so it is a direct socket read.
func (s *AnchorSide) ReadRTP(buf []byte) (int, error) { return s.rtpConn.Read(buf) }

// WriteRTP sends one plaintext RTP packet to the leg's current remote. For a plain leg
// the wire bytes are the plaintext, so it is a direct socket write. A nil remote (not
// yet negotiated, or held) drops the packet silently, matching copyUDP semantics.
func (s *AnchorSide) WriteRTP(pkt []byte) (int, error) {
	dst := s.loadRemoteRTP()
	if dst == nil {
		return len(pkt), nil
	}
	return s.rtpConn.WriteTo(pkt, dst)
}

// ReadRTCP reads one RTCP packet from the leg's RTCP socket (plaintext for a plain leg).
func (s *AnchorSide) ReadRTCP(buf []byte) (int, error) { return s.rtcpConn.Read(buf) }

// WriteRTCP sends one plaintext RTCP packet to the leg's current RTCP remote. A nil
// remote drops the packet silently.
func (s *AnchorSide) WriteRTCP(pkt []byte) (int, error) {
	dst := s.loadRemoteRTCP()
	if dst == nil {
		return len(pkt), nil
	}
	return s.rtcpConn.WriteTo(pkt, dst)
}

// Close satisfies MediaLeg; it closes the leg's sockets (idempotent via the OS).
func (s *AnchorSide) Close() { s.close() }

// Tap is a media fork to one application: two AnchorSides for caller and callee directions.
type Tap struct {
	appName      string
	callerStream *AnchorSide
	calleeStream *AnchorSide
	callerPair   PortPair
	calleePair   PortPair
}

func (t *Tap) close() {
	t.callerStream.close()
	t.calleeStream.close()
}

// MediaSession relays RTP and RTCP between two anchored sides, fanning out to zero or more taps.
//
// endpointSide is the plain endpoint anchor. When the endpoint is a secured WebRTC
// leg (STORY-001-019) endpointSide is nil and endpointLeg holds the secured leg
// instead; that leg is brought up and answered, but bridging it to pbxSide is
// STORY-001-021, so no relay runs for it yet.
type MediaSession struct {
	endpointSide *AnchorSide
	pbxSide      *AnchorSide
	endpointLeg  MediaLeg
	tapsMu       sync.Mutex
	taps         []*Tap
	closeOnce    sync.Once
}

// addTap registers a tap. Registration normally completes before relay starts,
// but teardown can read the tap list concurrently during mid-setup cancellation,
// so the slice is mutex-guarded.
func (m *MediaSession) addTap(t *Tap) {
	m.tapsMu.Lock()
	m.taps = append(m.taps, t)
	m.tapsMu.Unlock()
}

// tapList returns a snapshot of the registered taps for safe concurrent reads.
func (m *MediaSession) tapList() []*Tap {
	m.tapsMu.Lock()
	defer m.tapsMu.Unlock()
	out := make([]*Tap, len(m.taps))
	copy(out, m.taps)
	return out
}

// reanchor atomically swaps the relay destination on one anchor side without
// rebinding sockets or restarting goroutines. The relay picks up the new address
// on the very next packet.
func (m *MediaSession) reanchor(side *AnchorSide, rtp, rtcp *net.UDPAddr) {
	side.setRemote(rtp, rtcp)
}

// Close idempotently closes all sockets and the secured endpoint leg (when present),
// unblocking relay goroutines.
func (m *MediaSession) Close() {
	m.closeOnce.Do(func() {
		if m.endpointSide != nil {
			m.endpointSide.close()
		}
		if m.pbxSide != nil {
			m.pbxSide.close()
		}
		if m.endpointLeg != nil {
			m.endpointLeg.Close()
		}
		for _, t := range m.tapList() {
			t.close()
		}
	})
}

const rtpBufSize = 1500

// relay starts goroutines copying packets between the two sides, fanning out to taps.
// Returns when ctx is cancelled or sockets are closed.
func (m *MediaSession) relay(ctx context.Context) {
	// Snapshot tap slices at relay start — no mutation after this point.
	taps := m.tapList()
	callerTaps := make([]*AnchorSide, 0, len(taps))
	calleeTaps := make([]*AnchorSide, 0, len(taps))
	for _, t := range taps {
		callerTaps = append(callerTaps, t.callerStream)
		calleeTaps = append(calleeTaps, t.calleeStream)
	}

	var wg sync.WaitGroup
	wg.Add(4)

	// caller direction: endpoint-facing → PBX-facing
	go func() {
		defer wg.Done()
		copyUDPFanout(ctx, m.endpointSide.rtpConn, m.pbxSide.rtpConn, &m.pbxSide.remoteRTP, callerTaps)
	}()
	// callee direction: PBX-facing → endpoint-facing
	go func() {
		defer wg.Done()
		copyUDPFanout(ctx, m.pbxSide.rtpConn, m.endpointSide.rtpConn, &m.endpointSide.remoteRTP, calleeTaps)
	}()
	// RTCP caller direction
	go func() {
		defer wg.Done()
		copyUDP(ctx, m.endpointSide.rtcpConn, m.pbxSide.rtcpConn, &m.pbxSide.remoteRTCP)
	}()
	// RTCP callee direction
	go func() {
		defer wg.Done()
		copyUDP(ctx, m.pbxSide.rtcpConn, m.endpointSide.rtcpConn, &m.endpointSide.remoteRTCP)
	}()

	<-ctx.Done()
	m.Close()
	wg.Wait()
}

// copyUDPFanout reads packets from readConn, writes each to primary (loaded atomically
// per packet), then to each tap's RTP socket. A nil primary drops the packet. Tap write
// errors are logged and skipped.
func copyUDPFanout(ctx context.Context, readConn, writeConn *net.UDPConn, primary *atomic.Pointer[net.UDPAddr], tapSides []*AnchorSide) {
	buf := make([]byte, rtpBufSize)
	for {
		n, err := readConn.Read(buf)
		if err != nil {
			select {
			case <-ctx.Done():
			default:
				slog.Debug("relay read", "err", err)
			}
			return
		}
		pkt := buf[:n]

		dst := primary.Load()
		if dst != nil {
			if _, err := writeConn.WriteTo(pkt, dst); err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					slog.Debug("relay write primary", "dst", dst, "err", err)
					return
				}
			}
		}

		for _, tap := range tapSides {
			if tap == nil {
				continue
			}
			tapDst := tap.loadRemoteRTP()
			if tapDst == nil {
				continue
			}
			if _, err := tap.rtpConn.WriteTo(pkt, tapDst); err != nil {
				select {
				case <-ctx.Done():
				default:
					slog.Debug("tap write", "app", tap.localRTPPort, "err", err)
				}
			}
		}
	}
}

// bridgeLegs relays decrypted RTP and RTCP between two media legs in both directions,
// fanning RTP out to taps as plaintext. It is security-agnostic: each leg's ReadRTP/
// WriteRTP (and RTCP mirrors) applies that leg's own on-the-wire security, so the relay
// only ever moves plaintext and never references SRTP or MediaSecurity. Making the
// opposite leg SRTP later (SRTP↔SRTP) therefore requires no change here. callerTaps
// receive the a→b direction, calleeTaps the b→a direction. Returns when ctx is
// cancelled or a leg is closed.
func (m *MediaSession) bridgeLegs(ctx context.Context, a, b MediaLeg, callerTaps, calleeTaps []*AnchorSide) {
	var wg sync.WaitGroup
	wg.Add(4)

	// RTP a→b (e.g. webphone → PBX), fanning out to caller-direction taps.
	go func() {
		defer wg.Done()
		copyLegRTP(ctx, a, b, callerTaps)
	}()
	// RTP b→a (e.g. PBX → webphone), fanning out to callee-direction taps.
	go func() {
		defer wg.Done()
		copyLegRTP(ctx, b, a, calleeTaps)
	}()
	// RTCP a→b.
	go func() {
		defer wg.Done()
		copyLegRTCP(ctx, a, b)
	}()
	// RTCP b→a.
	go func() {
		defer wg.Done()
		copyLegRTCP(ctx, b, a)
	}()

	<-ctx.Done()
	m.Close()
	wg.Wait()
}

// copyLegRTP reads one decrypted RTP packet from src, writes it to dst (which applies
// dst's outbound security), then fans the plaintext out to each tap's RTP socket. A
// read or write error logs at debug and stops this direction only — best-effort,
// call-isolated. Tap write errors are logged and skipped, never blocking the primary.
func copyLegRTP(ctx context.Context, src, dst MediaLeg, tapSides []*AnchorSide) {
	buf := make([]byte, rtpBufSize)
	for {
		n, err := src.ReadRTP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
			default:
				slog.Debug("bridge read rtp", "err", err)
			}
			return
		}
		pkt := buf[:n]

		if _, err := dst.WriteRTP(pkt); err != nil {
			select {
			case <-ctx.Done():
			default:
				slog.Debug("bridge write rtp", "err", err)
			}
			return
		}

		for _, tap := range tapSides {
			if tap == nil {
				continue
			}
			tapDst := tap.loadRemoteRTP()
			if tapDst == nil {
				continue
			}
			if _, err := tap.rtpConn.WriteTo(pkt, tapDst); err != nil {
				select {
				case <-ctx.Done():
				default:
					slog.Debug("tap write", "app", tap.localRTPPort, "err", err)
				}
			}
		}
	}
}

// copyLegRTCP reads one decrypted RTCP packet from src and writes it to dst, applying
// dst's outbound security. A read or write error logs at debug and stops this direction
// only — best-effort, call-isolated.
func copyLegRTCP(ctx context.Context, src, dst MediaLeg) {
	buf := make([]byte, rtpBufSize)
	for {
		n, err := src.ReadRTCP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
			default:
				slog.Debug("bridge read rtcp", "err", err)
			}
			return
		}
		if _, err := dst.WriteRTCP(buf[:n]); err != nil {
			select {
			case <-ctx.Done():
			default:
				slog.Debug("bridge write rtcp", "err", err)
			}
			return
		}
	}
}

// copyUDP reads packets from readConn and writes to the destination loaded atomically
// from dst per packet. A nil destination drops the packet silently.
func copyUDP(ctx context.Context, readConn, writeConn *net.UDPConn, dst *atomic.Pointer[net.UDPAddr]) {
	buf := make([]byte, rtpBufSize)
	for {
		n, err := readConn.Read(buf)
		if err != nil {
			select {
			case <-ctx.Done():
			default:
				slog.Debug("relay read", "err", err)
			}
			return
		}
		addr := dst.Load()
		if addr == nil {
			continue
		}
		if _, err := writeConn.WriteTo(buf[:n], addr); err != nil {
			select {
			case <-ctx.Done():
			default:
				slog.Debug("relay write", "dst", addr, "err", err)
			}
			return
		}
	}
}

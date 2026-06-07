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
type MediaSession struct {
	endpointSide *AnchorSide
	pbxSide      *AnchorSide
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

// Close idempotently closes all sockets, unblocking relay goroutines.
func (m *MediaSession) Close() {
	m.closeOnce.Do(func() {
		m.endpointSide.close()
		m.pbxSide.close()
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
			tapDst := tap.loadRemoteRTP()
			if tap == nil || tapDst == nil {
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

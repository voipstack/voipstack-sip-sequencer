package recorder

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// Config holds runtime parameters for the recording application.
type Config struct {
	Listen    string // SIP UDP listen address, e.g. "0.0.0.0:5070"
	Dir       string // root folder; per-call sub-folders are created here
	MediaHost string // IP advertised in SDP answers and rtpdump headers
}

// callSession holds the RTP sockets open for one active call.
type callSession struct {
	conns []*net.UDPConn
}

// callRegistry maps SIP Call-ID to its open RTP sockets.
// Owned by Serve; accessed only through its methods.
type callRegistry struct {
	mu       sync.Mutex
	sessions map[string]*callSession
}

func newCallRegistry() *callRegistry {
	return &callRegistry{sessions: make(map[string]*callSession)}
}

func (r *callRegistry) add(callID string, s *callSession) {
	r.mu.Lock()
	r.sessions[callID] = s
	r.mu.Unlock()
}

// terminate closes all RTP sockets for callID and removes it from the registry.
func (r *callRegistry) terminate(callID string) {
	r.mu.Lock()
	s := r.sessions[callID]
	delete(r.sessions, callID)
	r.mu.Unlock()
	if s == nil {
		return
	}
	for _, c := range s.conns {
		c.Close()
	}
}

// closeAll terminates every active session (used on shutdown).
func (r *callRegistry) closeAll() {
	r.mu.Lock()
	ids := make([]string, 0, len(r.sessions))
	for id := range r.sessions {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		r.terminate(id)
	}
}

// Serve starts the SIP recording application and blocks until ctx is cancelled.
func Serve(ctx context.Context, cfg Config) error {
	ua, err := sipgo.NewUA()
	if err != nil {
		return fmt.Errorf("recorder UA: %w", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		return fmt.Errorf("recorder server: %w", err)
	}
	cli, err := sipgo.NewClient(ua)
	if err != nil {
		return fmt.Errorf("recorder client: %w", err)
	}

	listenHost, portStr, _ := net.SplitHostPort(cfg.Listen)
	sipPort, _ := strconv.Atoi(portStr)
	contactHost := listenHost
	if contactHost == "" || contactHost == "0.0.0.0" {
		contactHost = cfg.MediaHost
	}
	contact := sip.ContactHeader{Address: sip.Uri{Host: contactHost, Port: sipPort}}
	dsc := sipgo.NewDialogServerCache(cli, contact)

	reg := newCallRegistry()

	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		handleInvite(cfg, dsc, reg, req, tx)
	})
	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = dsc.ReadAck(req, tx)
	})
	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		callID := req.CallID().Value()
		_ = dsc.ReadBye(req, tx)
		reg.terminate(callID)
		slog.Info("call ended", "call_id", callID)
	})
	srv.OnCancel(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		_ = tx.Respond(res)
	})

	l, err := net.ListenPacket("udp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("recorder listen %s: %w", cfg.Listen, err)
	}

	slog.Info("recorder started", "listen", cfg.Listen, "dir", cfg.Dir, "media_host", cfg.MediaHost)

	go func() {
		<-ctx.Done()
		slog.Info("recorder shutting down")
		reg.closeAll()
		l.Close()
	}()

	err = srv.ServeUDP(l)
	slog.Info("recorder stopped")
	return err
}

func handleInvite(cfg Config, dsc *sipgo.DialogServerCache, reg *callRegistry, req *sip.Request, tx sip.ServerTransaction) {
	offer := req.Body()

	streams, err := parseOffer(offer)
	if err != nil {
		res := sip.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil)
		_ = tx.Respond(res)
		slog.Warn("INVITE rejected: bad offer", "call_id", req.CallID().Value(), "err", err)
		return
	}

	slog.Info("INVITE received", "call_id", req.CallID().Value(), "from", req.From(), "streams", len(streams))

	conns := make([]*net.UDPConn, len(streams))
	ports := make([]int, len(streams))
	for i := range streams {
		c, err := net.ListenUDP("udp", &net.UDPAddr{})
		if err != nil {
			for j := 0; j < i; j++ {
				conns[j].Close()
			}
			res := sip.NewResponseFromRequest(req, 500, "Server Error", nil)
			_ = tx.Respond(res)
			slog.Error("recorder: bind RTP socket", "err", err)
			return
		}
		conns[i] = c
		ports[i] = c.LocalAddr().(*net.UDPAddr).Port
	}

	answer, err := buildAnswer(offer, cfg.MediaHost, ports)
	if err != nil {
		for _, c := range conns {
			c.Close()
		}
		res := sip.NewResponseFromRequest(req, 500, "Server Error", nil)
		_ = tx.Respond(res)
		slog.Error("recorder: build answer", "err", err)
		return
	}

	dss, err := dsc.ReadInvite(req, tx)
	if err != nil {
		for _, c := range conns {
			c.Close()
		}
		slog.Error("recorder: ReadInvite", "err", err)
		return
	}

	if err := dss.Respond(200, "OK", answer); err != nil {
		for _, c := range conns {
			c.Close()
		}
		slog.Error("recorder: Respond 200", "err", err)
		return
	}

	// Folder named by X-Sequencer-Call-Id if present, otherwise SIP Call-ID.
	sipCallID := req.CallID().Value()
	folderID := sipCallID
	if hdr := req.GetHeader("X-Sequencer-Call-Id"); hdr != nil {
		if v := strings.TrimSpace(hdr.Value()); v != "" {
			folderID = v
		}
	}
	dir := filepath.Join(cfg.Dir, sanitizeName(folderID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		for _, c := range conns {
			c.Close()
		}
		slog.Error("recorder: mkdir", "dir", dir, "err", err)
		return
	}

	// Register so OnBye can close sockets.
	reg.add(sipCallID, &callSession{conns: conns})

	slog.Info("recording call", "call_id", sipCallID, "dir", dir, "streams", len(streams))

	// Open rtpdump files and start one recording goroutine per stream.
	startTime := time.Now()
	sec := uint32(startTime.Unix())
	usec := uint32(startTime.Nanosecond() / 1e3)
	for i, c := range conns {
		name := streamName(i)
		path := filepath.Join(dir, name+".rtpdump")
		f, err := os.Create(path)
		if err != nil {
			slog.Error("recorder: create file", "path", path, "err", err)
			continue
		}
		if _, err := f.Write(rtpdumpFileHeader(cfg.MediaHost, uint16(ports[i]), sec, usec)); err != nil {
			f.Close()
			slog.Error("recorder: write file header", "err", err)
			continue
		}
		slog.Info("stream recording started", "call_id", sipCallID, "stream", name, "path", path, "rtp_port", ports[i])
		go func(conn *net.UDPConn, file *os.File, start time.Time, stream string, callID string) {
			defer file.Close()
			recordLoop(stream, callID, conn, file, start)
		}(c, f, startTime, name, sipCallID)
	}

	// Block until Confirmed; required so sipgo does not race TerminateGracefully
	// against our Respond call (mirrors the fakeUAS pattern in the sequencer tests).
	stateCh := dss.StateRead()
	for {
		select {
		case s := <-stateCh:
			if s >= sip.DialogStateConfirmed {
				return
			}
		case <-tx.Done():
			return
		}
	}
}

func recordLoop(stream, callID string, conn *net.UDPConn, f *os.File, start time.Time) {
	buf := make([]byte, 1500)
	var pkts int
	for {
		n, err := conn.Read(buf)
		if err != nil {
			slog.Info("stream recording stopped", "call_id", callID, "stream", stream, "packets", pkts)
			return
		}
		pkts++
		offsetMs := uint32(time.Since(start).Milliseconds())
		rec := rtpdumpRecord(buf[:n], offsetMs)
		if _, err := f.Write(rec); err != nil {
			slog.Error("recorder: write record", "call_id", callID, "stream", stream, "err", err)
			return
		}
	}
}

func streamName(i int) string {
	switch i {
	case 0:
		return "caller"
	case 1:
		return "callee"
	default:
		return fmt.Sprintf("stream%d", i)
	}
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '/', '\\', '\x00':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

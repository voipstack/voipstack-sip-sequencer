package b2bua

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
	"github.com/voipstack/voipstack-sip-sequencer/internal/tlsprov"
)

// Engine owns the SIP UA, UDP listener, dialog caches, and active-call registry.
type Engine struct {
	cfg            config.Config
	ua             *sipgo.UserAgent
	srv            *sipgo.Server
	cli            *sipgo.Client
	dialogSrvCache *sipgo.DialogServerCache
	dialogCliCache *sipgo.DialogClientCache
	calls          *Registry
	metrics        MetricsSink
	tlsProvider    tlsprov.Provider
	tlsServerConf  *tls.Config
	wssServerConf  *tls.Config
	tlsDialers     map[string]*sipgo.DialogClientCache
	tlsUAs         []*sipgo.UserAgent
	runCtx         context.Context
	runCancel      context.CancelFunc
	legTimeout     time.Duration
	ports          *PortAllocator
	mediaHost      string
	obsListen      string
	obsServer      *http.Server
}

// Option customizes an Engine at construction. Options are applied after defaults,
// so they override them.
type Option func(*Engine)

// WithMetrics installs a MetricsSink, replacing the default noopMetrics.
func WithMetrics(s MetricsSink) Option {
	return func(e *Engine) { e.metrics = s }
}

// WithTLSProvider installs the TLS Provider so TLS listeners and dialers
// (STORY-001-014/015/016) can reach loaded certificate material. This story
// stores it only; the engine does not yet use it.
func WithTLSProvider(p tlsprov.Provider) Option {
	return func(e *Engine) { e.tlsProvider = p }
}

// New builds an Engine from cfg. The UDP listener is not opened until Run is called.
func New(cfg config.Config, opts ...Option) (*Engine, error) {
	host, portStr, err := net.SplitHostPort(cfg.SIP.Listen)
	if err != nil {
		return nil, fmt.Errorf("parse listen addr %q: %w", cfg.SIP.Listen, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("parse listen port %q: %w", portStr, err)
	}

	portMin, portMax, err := parsePortRange(cfg.RTP.PortRange)
	if err != nil {
		return nil, fmt.Errorf("rtp.port_range: %w", err)
	}

	ua, err := sipgo.NewUA()
	if err != nil {
		return nil, fmt.Errorf("create UA: %w", err)
	}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		return nil, fmt.Errorf("create SIP server: %w", err)
	}

	cli, err := sipgo.NewClient(ua, sipgo.WithClientHostname(host))
	if err != nil {
		return nil, fmt.Errorf("create SIP client: %w", err)
	}

	contactHDR := sip.ContactHeader{
		Address: sip.Uri{Host: host, Port: port},
	}

	e := &Engine{
		cfg:            cfg,
		ua:             ua,
		srv:            srv,
		cli:            cli,
		dialogSrvCache: sipgo.NewDialogServerCache(cli, contactHDR),
		dialogCliCache: sipgo.NewDialogClientCache(cli, contactHDR),
		calls:          &Registry{m: make(map[string]*Call), byDialog: make(map[string]*Call)},
		metrics:        noopMetrics{},
		tlsDialers:     map[string]*sipgo.DialogClientCache{},
		legTimeout:     32 * time.Second,
		ports:          newPortAllocator(portMin, portMax),
		mediaHost:      host,
		obsListen:      cfg.Observability.Listen,
	}
	for _, opt := range opts {
		opt(e)
	}

	// Build the inbound TLS server context once, before any socket binds, so a bad
	// certificate or policy aborts startup (fail-fast) rather than running degraded.
	if cfg.TLS.Listen != "" {
		if e.tlsProvider == nil {
			return nil, fmt.Errorf("tls.listen %q configured but no TLS provider", cfg.TLS.Listen)
		}
		if cfg.TLS.Resolved == nil {
			return nil, fmt.Errorf("tls.listen %q has no resolved profile", cfg.TLS.Listen)
		}
		conf, err := e.tlsProvider.ServerConfig(*cfg.TLS.Resolved)
		if err != nil {
			return nil, fmt.Errorf("build tls server context: %w", err)
		}
		e.tlsServerConf = conf
	}

	// Build the inbound WSS server context the same way (fail-fast), mirroring the
	// tls.listen block. WSS reuses the audited TLS profile model verbatim.
	if cfg.WSS.Listen != "" {
		if e.tlsProvider == nil {
			return nil, fmt.Errorf("wss.listen %q configured but no TLS provider", cfg.WSS.Listen)
		}
		if cfg.WSS.Resolved == nil {
			return nil, fmt.Errorf("wss.listen %q has no resolved profile", cfg.WSS.Listen)
		}
		conf, err := e.tlsProvider.ServerConfig(*cfg.WSS.Resolved)
		if err != nil {
			return nil, fmt.Errorf("build wss server context: %w", err)
		}
		e.wssServerConf = conf
	}

	// Build one outbound TLS dialer per distinct profile. sipgo binds the outbound
	// *tls.Config at the UserAgent (not per request), so each profile needs its own
	// UA+Client+DialogClientCache. Endpoints naming the same profile share one dialer,
	// hence one loaded certificate. A bad client context aborts startup (fail-fast).
	outbound := map[string]*config.ResolvedTLSProfile{}
	firstEndpoint := ""
	for i := range cfg.Sequence {
		app := cfg.Sequence[i]
		if app.Transport != config.TransportTLS || app.Resolved == nil {
			continue
		}
		if _, ok := outbound[app.Resolved.Name]; !ok {
			outbound[app.Resolved.Name] = app.Resolved
			if firstEndpoint == "" {
				firstEndpoint = fmt.Sprintf("sequence[%d] %q", i, app.Name)
			}
		}
	}
	if cfg.NextHop.Transport == config.TransportTLS && cfg.NextHop.Resolved != nil {
		if _, ok := outbound[cfg.NextHop.Resolved.Name]; !ok {
			outbound[cfg.NextHop.Resolved.Name] = cfg.NextHop.Resolved
			if firstEndpoint == "" {
				firstEndpoint = "next_hop"
			}
		}
	}
	if len(outbound) > 0 && e.tlsProvider == nil {
		return nil, fmt.Errorf("%s uses transport tls but no TLS provider", firstEndpoint)
	}
	for name, rp := range outbound {
		conf, err := e.tlsProvider.ClientConfig(*rp)
		if err != nil {
			return nil, fmt.Errorf("build tls client context for profile %q: %w", name, err)
		}
		tlsUA, err := sipgo.NewUA(sipgo.WithUserAgenTLSConfig(conf))
		if err != nil {
			return nil, fmt.Errorf("create TLS UA for profile %q: %w", name, err)
		}
		tlsCli, err := sipgo.NewClient(tlsUA, sipgo.WithClientHostname(host))
		if err != nil {
			return nil, fmt.Errorf("create TLS client for profile %q: %w", name, err)
		}
		e.tlsDialers[name] = sipgo.NewDialogClientCache(tlsCli, contactHDR)
		e.tlsUAs = append(e.tlsUAs, tlsUA)
	}

	return e, nil
}

// Run registers SIP handlers and starts the plain UDP listener on cfg.SIP.Listen,
// plus — when a tls.listen is configured — the inbound TLS listener in parallel on
// the same server. It blocks until ctx is cancelled or a listener fails.
func (e *Engine) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	e.runCtx = ctx
	e.runCancel = cancel
	defer cancel()

	e.srv.OnInvite(e.handleInvite)
	e.srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = e.dialogSrvCache.ReadAck(req, tx)
	})
	e.srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		if err := e.dialogSrvCache.ReadBye(req, tx); err == nil {
			return
		}
		if err := e.dialogCliCache.ReadBye(req, tx); err == nil {
			return
		}
		res := sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil)
		_ = tx.Respond(res)
	})
	e.srv.OnRefer(e.handleRefer)
	// All methods not explicitly managed above are forwarded to cfg.NextHop.
	e.srv.OnNoRoute(e.proxyUnmanaged)

	if e.obsListen != "" {
		e.startObservability(ctx)
	}

	// Plain UDP and TLS run as independent sockets on the shared server. errgroup
	// ties them to one context: if either fails (e.g. a TLS bind error at startup),
	// the sibling is cancelled and Run returns that error (fail-fast).
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return e.srv.ListenAndServe(gctx, "udp", e.cfg.SIP.Listen)
	})
	if e.tlsServerConf != nil {
		g.Go(func() error {
			return e.serveTLS(gctx, e.cfg.TLS.Listen, e.tlsServerConf)
		})
	}
	// WebSocket listeners are additive siblings. sipgo owns the ws/wss upgrade, the
	// sip subprotocol, and frame↔SIP; a ws-accepted request flows through the same
	// handlers as UDP. A clean gctx cancel closes the listener and returns nil.
	if e.cfg.WS.Listen != "" {
		g.Go(func() error {
			return e.srv.ListenAndServe(gctx, "ws", e.cfg.WS.Listen)
		})
	}
	if e.wssServerConf != nil {
		g.Go(func() error {
			return e.srv.ListenAndServeTLS(gctx, "wss", e.cfg.WSS.Listen, e.wssServerConf)
		})
	}
	return g.Wait()
}

// serveTLS binds the inbound TLS listener on addr and serves SIP over it on the
// shared server. Binding is synchronous so a bad bind fails Run fast; each
// connection's handshake runs on the server's own per-connection goroutine, so a
// slow or failed handshake never blocks other accepts or the plain listener. A
// rejected handshake is logged with the peer address and a sanitized reason — never
// certificate or key bytes. A clean ctx cancellation returns nil, matching the
// plain listener's shutdown semantics.
func (e *Engine) serveTLS(ctx context.Context, addr string, conf *tls.Config) error {
	ln, err := tls.Listen("tcp", addr, conf)
	if err != nil {
		return fmt.Errorf("listen tls %q: %w", addr, err)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	err = e.srv.ServeTLS(&auditListener{Listener: ln, log: slog.Default()})
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// auditListener wraps a TLS listener so each accepted connection carries the audit
// hook that logs its own handshake failure when the SIP server first reads from it.
type auditListener struct {
	net.Listener
	log *slog.Logger
}

func (l *auditListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &auditConn{Conn: c, log: l.log}, nil
}

// auditConn logs a rejected TLS handshake once, with the peer address and a
// sanitized reason. crypto/tls performs the server handshake lazily on the first
// Read; when it fails the connection's handshake is still incomplete and Read
// returns the handshake error, whose text carries no certificate or key bytes.
type auditConn struct {
	net.Conn
	log    *slog.Logger
	logged bool
}

func (c *auditConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err != nil && err != io.EOF && !errors.Is(err, io.EOF) && !c.logged {
		if tc, ok := c.Conn.(*tls.Conn); ok && !tc.ConnectionState().HandshakeComplete {
			c.logged = true
			c.log.Warn("tls handshake rejected", "peer", c.Conn.RemoteAddr().String(), "reason", err.Error())
		}
	}
	return n, err
}

// obsExposer is the metrics-backend surface the observability server needs: a
// Prometheus handler plus a way to bind the live call-source gauges.
type obsExposer interface {
	BindCallSource(CallSource)
	Handler() http.Handler
}

// startObservability serves /metrics (when the sink exposes one) and /health on
// e.obsListen, on its own goroutine off the SIP/relay path. A startup bind
// failure is logged; the engine keeps running its SIP service.
func (e *Engine) startObservability(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	if exposer, ok := e.metrics.(obsExposer); ok {
		exposer.BindCallSource(e.calls)
		mux.Handle("/metrics", exposer.Handler())
	}

	e.obsServer = &http.Server{Addr: e.obsListen, Handler: mux}
	go func() {
		slog.Info("observability server listening", "addr", e.obsListen)
		if err := e.obsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("observability server", "addr", e.obsListen, "err", err)
		}
	}()
}

// Shutdown tears down all active calls, stops the observability server, and
// stops the SIP server.
func (e *Engine) Shutdown() error {
	if e.obsServer != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = e.obsServer.Shutdown(shutCtx)
		cancel()
	}
	e.calls.each(func(c *Call) {
		c.teardown("engine shutdown")
	})
	if e.runCancel != nil {
		e.runCancel()
	}
	for _, ua := range e.tlsUAs {
		_ = ua.Close()
	}
	return e.srv.Close()
}

func newCallID() string {
	return uuid.NewString()
}

func newLegID() string {
	return uuid.NewString()
}

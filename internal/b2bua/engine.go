package b2bua

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
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
		legTimeout:     32 * time.Second,
		ports:          newPortAllocator(portMin, portMax),
		mediaHost:      host,
		obsListen:      cfg.Observability.Listen,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e, nil
}

// Run registers SIP handlers, starts the UDP listener on cfg.SIP.Listen, and
// blocks until ctx is cancelled.
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

	return e.srv.ListenAndServe(ctx, "udp", e.cfg.SIP.Listen)
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
	return e.srv.Close()
}

func newCallID() string {
	return uuid.NewString()
}

func newLegID() string {
	return uuid.NewString()
}

// Copyright 2023 Sauce Labs Inc. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package forwarder

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/saucelabs/forwarder/httplog"
	"github.com/saucelabs/forwarder/internal/martian"
	"github.com/saucelabs/forwarder/internal/martian/fifo"
	"github.com/saucelabs/forwarder/internal/martian/httpspec"
	"github.com/saucelabs/forwarder/log"
	"github.com/saucelabs/forwarder/middleware"
	"github.com/saucelabs/forwarder/pac"
	"github.com/saucelabs/forwarder/ratelimit"
	"github.com/saucelabs/forwarder/ruleset"
)

type ProxyLocalhostMode string

const (
	DenyProxyLocalhost   ProxyLocalhostMode = "deny"
	AllowProxyLocalhost  ProxyLocalhostMode = "allow"
	DirectProxyLocalhost ProxyLocalhostMode = "direct"
)

func (m *ProxyLocalhostMode) UnmarshalText(text []byte) error {
	switch ProxyLocalhostMode(text) {
	case DenyProxyLocalhost, AllowProxyLocalhost, DirectProxyLocalhost:
		*m = ProxyLocalhostMode(text)
		return nil
	default:
		return fmt.Errorf("invalid mode: %s", text)
	}
}

func (m ProxyLocalhostMode) String() string {
	return string(m)
}

func (m ProxyLocalhostMode) isValid() bool {
	switch m {
	case DenyProxyLocalhost, AllowProxyLocalhost, DirectProxyLocalhost:
		return true
	default:
		return false
	}
}

type ProxyFunc func(*http.Request) (*url.URL, error)

// Alias all martian types to avoid exposing them.
type (
	RequestModifier         = martian.RequestModifier
	ResponseModifier        = martian.ResponseModifier
	RequestResponseModifier = martian.RequestResponseModifier
	RequestModifierFunc     = martian.RequestModifierFunc
	ResponseModifierFunc    = martian.ResponseModifierFunc
)

type HTTPProxyConfig struct {
	HTTPServerConfig
	Name                   string
	MITM                   *MITMConfig
	MITMDomains            *ruleset.RegexpMatcher
	ProxyLocalhost         ProxyLocalhostMode
	UpstreamProxy          *url.URL
	UpstreamProxyFunc      ProxyFunc
	DenyDomains            *ruleset.RegexpMatcher
	DirectDomains          *ruleset.RegexpMatcher
	RequestIDHeader        string
	RequestModifiers       []RequestModifier
	ResponseModifiers      []ResponseModifier
	ConnectRequestModifier func(*http.Request) error
	ConnectPassthrough     bool
	CloseAfterReply        bool
	ReadLimit              SizeSuffix
	WriteLimit             SizeSuffix

	// TestingHTTPHandler uses Martian's [http.Handler] implementation
	// over [http.Server] instead of the default TCP server.
	TestingHTTPHandler bool
}

func DefaultHTTPProxyConfig() *HTTPProxyConfig {
	return &HTTPProxyConfig{
		HTTPServerConfig: HTTPServerConfig{
			Protocol:          HTTPScheme,
			Addr:              ":3128",
			ReadHeaderTimeout: 1 * time.Minute,
			LogHTTPMode:       httplog.Errors,
		},
		Name:            "forwarder",
		ProxyLocalhost:  DenyProxyLocalhost,
		RequestIDHeader: "X-Request-Id",
	}
}

func (c *HTTPProxyConfig) Validate() error {
	if err := c.HTTPServerConfig.Validate(); err != nil {
		return err
	}
	if c.Protocol != HTTPScheme && c.Protocol != HTTPSScheme {
		return fmt.Errorf("unsupported protocol: %s", c.Protocol)
	}
	if !c.ProxyLocalhost.isValid() {
		return fmt.Errorf("unsupported proxy_localhost: %s", c.ProxyLocalhost)
	}
	if err := validateProxyURL(c.UpstreamProxy); err != nil {
		return fmt.Errorf("upstream_proxy_uri: %w", err)
	}

	return nil
}

type HTTPProxy struct {
	config     HTTPProxyConfig
	pac        PACResolver
	creds      *CredentialsMatcher
	transport  http.RoundTripper
	log        log.Logger
	metrics    *httpProxyMetrics
	proxy      *martian.Proxy
	mitmCACert *x509.Certificate
	proxyFunc  ProxyFunc
	listener   net.Listener

	TLSConfig *tls.Config
}

// NewHTTPProxy creates a new HTTP proxy.
// It is the caller's responsibility to call Close on the returned server.
func NewHTTPProxy(cfg *HTTPProxyConfig, pr PACResolver, cm *CredentialsMatcher, rt http.RoundTripper, log log.Logger) (*HTTPProxy, error) {
	hp, err := newHTTPProxy(cfg, pr, cm, rt, log)
	if err != nil {
		return nil, err
	}

	if hp.config.Protocol == HTTPSScheme {
		if err := hp.configureHTTPS(); err != nil {
			return nil, err
		}
	}

	l, err := hp.listen()
	if err != nil {
		return nil, err
	}
	hp.listener = l

	hp.log.Infof("PROXY server listen address=%s protocol=%s", l.Addr(), hp.config.Protocol)

	return hp, nil
}

// NewHTTPProxyHandler is like NewHTTPProxy but returns http.Handler instead of *HTTPProxy.
func NewHTTPProxyHandler(cfg *HTTPProxyConfig, pr PACResolver, cm *CredentialsMatcher, rt http.RoundTripper, log log.Logger) (http.Handler, error) {
	hp, err := newHTTPProxy(cfg, pr, cm, rt, log)
	if err != nil {
		return nil, err
	}

	return hp.handler(), nil
}

func newHTTPProxy(cfg *HTTPProxyConfig, pr PACResolver, cm *CredentialsMatcher, rt http.RoundTripper, log log.Logger) (*HTTPProxy, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.UpstreamProxy != nil && pr != nil {
		return nil, fmt.Errorf("cannot use both upstream proxy and PAC")
	}

	// If not set, use http.DefaultTransport.
	if rt == nil {
		log.Infof("HTTP transport not configured, using standard library default")
		rt = http.DefaultTransport.(*http.Transport).Clone()
	} else if tr, ok := rt.(*http.Transport); !ok {
		log.Debugf("using custom HTTP transport %T", rt)
	} else if tr.TLSClientConfig != nil && tr.TLSClientConfig.RootCAs != nil {
		log.Infof("using custom root CA certificates")
	}
	hp := &HTTPProxy{
		config:    *cfg,
		pac:       pr,
		creds:     cm,
		transport: rt,
		log:       log,
		metrics:   newMetrics(cfg.PromRegistry, cfg.PromNamespace),
	}

	if err := hp.configureProxy(); err != nil {
		return nil, err
	}

	return hp, nil
}

func (hp *HTTPProxy) configureHTTPS() error {
	if hp.config.CertFile == "" && hp.config.KeyFile == "" {
		hp.log.Infof("no TLS certificate provided, using self-signed certificate")
	} else {
		hp.log.Debugf("loading TLS certificate from %s and %s", hp.config.CertFile, hp.config.KeyFile)
	}

	hp.TLSConfig = httpsTLSConfigTemplate()

	return hp.config.ConfigureTLSConfig(hp.TLSConfig)
}

func (hp *HTTPProxy) configureProxy() error {
	hp.proxy = martian.NewProxy()

	if hp.config.MITM != nil {
		hp.log.Infof("using MITM")
		mc, err := newMartianMITMConfig(hp.config.MITM)
		if err != nil {
			return fmt.Errorf("mitm: %w", err)
		}
		hp.proxy.SetMITM(mc)
		hp.mitmCACert = mc.CACert()

		if hp.config.MITMDomains != nil {
			hp.proxy.MITMFilter = func(req *http.Request) bool {
				return hp.config.MITMDomains.Match(req.URL.Hostname())
			}
		}
	}

	hp.proxy.AllowHTTP = true
	hp.proxy.RequestIDHeader = hp.config.RequestIDHeader
	hp.proxy.ConnectRequestModifier = hp.config.ConnectRequestModifier
	hp.proxy.ConnectPassthrough = hp.config.ConnectPassthrough
	hp.proxy.WithoutWarning = true
	hp.proxy.ErrorResponse = hp.errorResponse
	hp.proxy.CloseAfterReply = hp.config.CloseAfterReply
	hp.proxy.ReadTimeout = hp.config.ReadTimeout
	hp.proxy.ReadHeaderTimeout = hp.config.ReadHeaderTimeout
	hp.proxy.WriteTimeout = hp.config.WriteTimeout
	// Martian has an intertwined logic for setting http.Transport and the dialer.
	// The dialer is wrapped, so that additional syscalls are made to the dialed connections.
	// As a result the dialer needs to be reset.
	if tr, ok := hp.transport.(*http.Transport); ok {
		// Note: The order matters. DialContext needs to be set first.
		// SetRoundTripper overwrites tr.DialContext with hp.proxy.dial.
		hp.proxy.SetDialContext(tr.DialContext)
		hp.proxy.SetRoundTripper(tr)
	} else {
		hp.proxy.SetRoundTripper(hp.transport)
	}

	switch {
	case hp.config.UpstreamProxyFunc != nil:
		hp.log.Infof("using external proxy function")
		hp.proxyFunc = hp.config.UpstreamProxyFunc
	case hp.config.UpstreamProxy != nil:
		u := hp.upstreamProxyURL()
		hp.log.Infof("using upstream proxy: %s", u.Redacted())
		hp.proxyFunc = http.ProxyURL(u)
	case hp.pac != nil:
		hp.log.Infof("using PAC proxy")
		hp.proxyFunc = hp.pacProxy
	default:
		hp.log.Infof("no upstream proxy specified")
	}

	if hp.config.DirectDomains != nil {
		hp.proxyFunc = hp.directDomains(hp.proxyFunc)
	}

	hp.log.Infof("localhost proxying mode=%s", hp.config.ProxyLocalhost)
	if hp.config.ProxyLocalhost == DirectProxyLocalhost {
		hp.proxyFunc = hp.directLocalhost(hp.proxyFunc)
	}
	hp.proxy.SetUpstreamProxyFunc(hp.proxyFunc)

	mw := hp.middlewareStack()
	hp.proxy.SetRequestModifier(mw)
	hp.proxy.SetResponseModifier(mw)

	return nil
}

func (hp *HTTPProxy) upstreamProxyURL() *url.URL {
	proxyURL := new(url.URL)
	*proxyURL = *hp.config.UpstreamProxy

	if proxyURL.User == nil {
		if u := hp.creds.MatchURL(proxyURL); u != nil {
			proxyURL.User = u
		}
	}

	return proxyURL
}

func (hp *HTTPProxy) pacProxy(r *http.Request) (*url.URL, error) {
	s, err := hp.pac.FindProxyForURL(r.URL, "")
	if err != nil {
		return nil, err
	}

	p, err := pac.Proxies(s).First()
	if err != nil {
		return nil, err
	}

	proxyURL := p.URL()
	if u := hp.creds.MatchURL(proxyURL); u != nil {
		proxyURL.User = u
	}

	return proxyURL, nil
}

func (hp *HTTPProxy) middlewareStack() martian.RequestResponseModifier {
	// Wrap stack in a group so that we can run security checks before the httpspec modifiers.
	topg := fifo.NewGroup()
	if hp.config.BasicAuth != nil {
		hp.log.Infof("basic auth enabled")
		topg.AddRequestModifier(hp.basicAuth(hp.config.BasicAuth))
	}
	if hp.config.ProxyLocalhost == DenyProxyLocalhost {
		topg.AddRequestModifier(hp.denyLocalhost())
	}
	if hp.config.DenyDomains != nil {
		topg.AddRequestModifier(hp.denyDomains(hp.config.DenyDomains))
	}

	// stack contains the request/response modifiers in the order they are applied.
	// fg is the inner stack that is executed after the core request modifiers and before the core response modifiers.
	stack, fg := httpspec.NewStack(hp.config.Name)
	topg.AddRequestModifier(stack)
	topg.AddResponseModifier(stack)

	for _, m := range hp.config.RequestModifiers {
		fg.AddRequestModifier(m)
	}

	for _, m := range hp.config.ResponseModifiers {
		fg.AddResponseModifier(m)
	}

	if hp.config.LogHTTPMode != httplog.None {
		lf := httplog.NewLogger(hp.log.Infof, hp.config.LogHTTPMode).LogFunc()
		fg.AddRequestModifier(lf)
		fg.AddResponseModifier(lf)
	}

	if hp.config.PromRegistry != nil {
		p := middleware.NewPrometheus(hp.config.PromRegistry, hp.config.PromNamespace)
		stack.AddRequestModifier(p)
		stack.AddResponseModifier(p)
	}

	fg.AddRequestModifier(martian.RequestModifierFunc(hp.setBasicAuth))
	fg.AddRequestModifier(martian.RequestModifierFunc(setEmptyUserAgent))

	return topg.ToImmutable()
}

func (hp *HTTPProxy) abortIf(condition func(r *http.Request) bool, response func(*http.Request) *http.Response, returnErr error) martian.RequestModifier {
	return martian.RequestModifierFunc(func(req *http.Request) error {
		if !condition(req) {
			return nil
		}

		lf := httplog.NewLogger(hp.log.Infof, hp.config.LogHTTPMode).LogFunc()
		if err := lf.ModifyRequest(req); err != nil {
			hp.log.Errorf("got error while logging request: %s", err)
		}

		res := response(req)
		defer res.Body.Close()
		res.Close = true // hijacked connection is closed by Martian in handleLoop()

		if err := lf.ModifyResponse(res); err != nil {
			hp.log.Errorf("got error while logging response: %s", err)
		}

		session := martian.NewContext(req).Session()
		var (
			brw *bufio.ReadWriter
			rw  http.ResponseWriter
			err error
		)
		_, brw, err = session.Hijack()
		if err == nil {
			hp.writeErrorResponseToBuffer(res, brw)
		} else if errors.Is(err, http.ErrNotSupported) {
			rw, err = session.HijackResponseWriter()
			if err == nil {
				hp.writeErrorResponseToResponseWriter(res, rw)
			}
		}
		if err != nil {
			panic(err)
		}

		return returnErr
	})
}

func (hp *HTTPProxy) writeErrorResponseToBuffer(res *http.Response, brw *bufio.ReadWriter) {
	res.Write(brw) //nolint:errcheck // it's a buffer
	if err := brw.Flush(); err != nil {
		hp.log.Errorf("got error while flushing error response: %s", err)
	}
}

func (hp *HTTPProxy) writeErrorResponseToResponseWriter(res *http.Response, rw http.ResponseWriter) {
	header := rw.Header()
	for k, vv := range res.Header {
		for _, v := range vv {
			header.Add(k, v)
		}
	}
	if res.Close {
		header.Set("Connection", "close")
	}
	rw.WriteHeader(res.StatusCode)

	if _, err := io.Copy(rw, res.Body); err != nil {
		hp.log.Errorf("got error while writing error response: %s", err)
	}
}

func (hp *HTTPProxy) basicAuth(u *url.Userinfo) martian.RequestModifier {
	user := u.Username()
	pass, _ := u.Password()
	ba := middleware.NewProxyBasicAuth()

	return hp.abortIf(func(req *http.Request) bool {
		return !ba.AuthenticatedRequest(req, user, pass)
	}, unauthorizedResponse, errors.New("basic auth required"))
}

func (hp *HTTPProxy) denyLocalhost() martian.RequestModifier {
	return hp.abortIf(hp.isLocalhost, func(req *http.Request) *http.Response {
		return hp.errorResponse(req, ErrProxyLocalhost)
	}, errors.New("localhost access denied"))
}

func (hp *HTTPProxy) denyDomains(r *ruleset.RegexpMatcher) martian.RequestModifier {
	return hp.abortIf(func(req *http.Request) bool {
		return r.Match(req.URL.Hostname())
	}, func(req *http.Request) *http.Response {
		return hp.errorResponse(req, ErrProxyDenied)
	}, errors.New("domain access denied"))
}

func (hp *HTTPProxy) directDomains(fn ProxyFunc) ProxyFunc {
	if fn == nil {
		return nil
	}

	return func(req *http.Request) (*url.URL, error) {
		if hp.config.DirectDomains.Match(req.URL.Hostname()) {
			return nil, nil
		}
		return fn(req)
	}
}

func (hp *HTTPProxy) directLocalhost(fn ProxyFunc) ProxyFunc {
	if fn == nil {
		return nil
	}

	return func(req *http.Request) (*url.URL, error) {
		if hp.isLocalhost(req) {
			return nil, nil
		}
		return fn(req)
	}
}

// nopResolver is a dns resolver that does not ever dial.
// It uses only the local resolver.
// It will look for the address in the /etc/hosts file.
var localhostResolver = nopResolver() //nolint:gochecknoglobals // This is a local resolver.

func (hp *HTTPProxy) isLocalhost(req *http.Request) bool {
	h := req.URL.Hostname()

	// Plain old localhost.
	if h == "localhost" {
		return true
	}

	if addrs, err := localhostResolver.LookupHost(context.Background(), h); err == nil {
		if ip := net.ParseIP(addrs[0]); ip != nil {
			return ip.IsLoopback()
		}
	}

	return false
}

func (hp *HTTPProxy) setBasicAuth(req *http.Request) error {
	if req.Header.Get("Authorization") == "" {
		if u := hp.creds.MatchURL(req.URL); u != nil {
			p, _ := u.Password()
			req.SetBasicAuth(u.Username(), p)
		}
	}

	return nil
}

func setEmptyUserAgent(req *http.Request) error {
	if _, ok := req.Header["User-Agent"]; !ok {
		// If the outbound request doesn't have a User-Agent header set,
		// don't send the default Go HTTP client User-Agent.
		req.Header.Set("User-Agent", "")
	}
	return nil
}

func (hp *HTTPProxy) MITMCACert() *x509.Certificate {
	return hp.mitmCACert
}

func (hp *HTTPProxy) ProxyFunc() ProxyFunc {
	return hp.proxyFunc
}

func (hp *HTTPProxy) handler() http.Handler {
	return hp.proxy.Handler()
}

func (hp *HTTPProxy) Run(ctx context.Context) error {
	var srv *http.Server

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		<-ctx.Done()
		if srv != nil {
			if err := srv.Shutdown(context.Background()); err != nil {
				hp.log.Errorf("failed to shutdown server error=%s", err)
			}
		} else {
			hp.Close()
		}
	}()

	var srvErr error
	if hp.config.TestingHTTPHandler {
		hp.log.Infof("using http handler")
		srv = &http.Server{
			Handler:           hp.handler(),
			ReadTimeout:       hp.config.ReadTimeout,
			ReadHeaderTimeout: hp.config.ReadHeaderTimeout,
			WriteTimeout:      hp.config.WriteTimeout,
		}
		srvErr = srv.Serve(hp.listener)
	} else {
		srvErr = hp.proxy.Serve(hp.listener)
	}
	if srvErr != nil {
		if errors.Is(srvErr, net.ErrClosed) {
			srvErr = nil
		}
		return srvErr
	}

	wg.Wait()
	return nil
}

func (hp *HTTPProxy) listen() (net.Listener, error) {
	listener, err := net.Listen("tcp", hp.config.Addr)
	if err != nil {
		return nil, fmt.Errorf("failed to open listener on address %s: %w", hp.config.Addr, err)
	}

	if rl, wl := int64(hp.config.ReadLimit), int64(hp.config.WriteLimit); rl > 0 || wl > 0 {
		// Notice that the ReadLimit stands for the read limit *from* a proxy, and the WriteLimit
		// stands for the write limit *to* a proxy, thus the ReadLimit is in fact
		// a txBandwidth and the WriteLimit is a rxBandwidth.
		listener = ratelimit.NewListener(listener, wl, rl)
	}

	switch hp.config.Protocol {
	case HTTPScheme:
		return listener, nil
	case HTTPSScheme, HTTP2Scheme:
		return tls.NewListener(listener, hp.TLSConfig), nil
	default:
		listener.Close()
		return nil, fmt.Errorf("invalid protocol %q", hp.config.Protocol)
	}
}

// Addr returns the address the server is listening on.
func (hp *HTTPProxy) Addr() string {
	return hp.listener.Addr().String()
}

func (hp *HTTPProxy) Close() error {
	err := hp.listener.Close()
	hp.proxy.Close()
	return err
}

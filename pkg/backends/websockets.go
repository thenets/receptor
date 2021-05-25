// +build !no_websocket_backend
// +build !no_backends

package backends

import (
	"context"
	"crypto/tls"
	"fmt"
	"github.com/ghjm/cmdline"
	"github.com/gorilla/websocket"
	"github.com/project-receptor/receptor/pkg/logger"
	"github.com/project-receptor/receptor/pkg/netceptor"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// WebsocketDialer implements Backend for outbound Websocket
type WebsocketDialer struct {
	address     string
	origin      string
	redial      bool
	tlscfg      *tls.Config
	extraHeader string
}

// NewWebsocketDialer instantiates a new WebsocketDialer backend
func NewWebsocketDialer(address string, tlscfg *tls.Config, extraHeader string, redial bool) (*WebsocketDialer, error) {
	addrURL, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	httpScheme := "http"
	if addrURL.Scheme == "wss" {
		httpScheme = "https"
	}
	wd := WebsocketDialer{
		address:     address,
		origin:      fmt.Sprintf("%s://%s", httpScheme, addrURL.Host),
		redial:      redial,
		tlscfg:      tlscfg,
		extraHeader: extraHeader,
	}
	return &wd, nil
}

// Start runs the given session function over this backend service
func (b *WebsocketDialer) Start(ctx context.Context) (chan netceptor.BackendSession, error) {
	return dialerSession(ctx, b.redial, 5*time.Second,
		func(closeChan chan struct{}) (netceptor.BackendSession, error) {
			dialer := websocket.Dialer{
				TLSClientConfig: b.tlscfg,
				Proxy:           http.ProxyFromEnvironment,
			}
			header := make(http.Header, 0)
			if b.extraHeader != "" {
				extraHeaderParts := strings.SplitN(b.extraHeader, ":", 2)
				header.Add(http.CanonicalHeaderKey(extraHeaderParts[0]), extraHeaderParts[1])
			}
			header.Add(http.CanonicalHeaderKey("origin"), b.origin)
			conn, _, err := dialer.DialContext(ctx, b.address, header)
			if err != nil {
				return nil, err
			}
			ns := newWebsocketSession(conn, closeChan)
			return ns, nil
		})
}

// WebsocketListener implements Backend for inbound Websocket
type WebsocketListener struct {
	address string
	tlscfg  *tls.Config
	li      net.Listener
	server  *http.Server
}

// NewWebsocketListener instantiates a new WebsocketListener backend
func NewWebsocketListener(address string, tlscfg *tls.Config) (*WebsocketListener, error) {
	ul := WebsocketListener{
		address: address,
		tlscfg:  tlscfg,
		li:      nil,
	}
	return &ul, nil
}

// Addr returns the network address the listener is listening on
func (b *WebsocketListener) Addr() net.Addr {
	if b.li == nil {
		return nil
	}
	return b.li.Addr()
}

// Start runs the given session function over the WebsocketListener backend
func (b *WebsocketListener) Start(ctx context.Context) (chan netceptor.BackendSession, error) {
	var err error
	sessChan := make(chan netceptor.BackendSession)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var upgrader = websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			logger.Error("Error upgrading websocket connection: %s\n", err)
			return
		}
		ws := newWebsocketSession(conn, nil)
		sessChan <- ws
	})
	b.li, err = net.Listen("tcp", b.address)
	if err != nil {
		return nil, err
	}
	go func() {
		var err error
		b.server = &http.Server{
			Addr:    b.address,
			Handler: mux,
		}
		if b.tlscfg == nil {
			err = b.server.Serve(b.li)
		} else {
			b.server.TLSConfig = b.tlscfg
			err = b.server.ServeTLS(b.li, "", "")
		}
		if err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server error: %s\n", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = b.server.Close()
	}()
	if err == nil {
		logger.Debug("Listening on Websocket %s\n", b.Addr().String())
	}
	return sessChan, nil
}

// WebsocketSession implements BackendSession for WebsocketDialer and WebsocketListener
type WebsocketSession struct {
	conn            *websocket.Conn
	recvChan        chan *recvResult
	closeChan       chan struct{}
	closeChanCloser sync.Once
}

type recvResult struct {
	data []byte
	err  error
}

func newWebsocketSession(conn *websocket.Conn, closeChan chan struct{}) *WebsocketSession {
	ws := &WebsocketSession{
		conn:            conn,
		recvChan:        make(chan *recvResult),
		closeChan:       closeChan,
		closeChanCloser: sync.Once{},
	}
	go ws.recvChannelizer()
	return ws
}

// recvChannelizer receives messages and pushes them to a channel.
func (ns *WebsocketSession) recvChannelizer() {
	for {
		_, data, err := ns.conn.ReadMessage()
		ns.recvChan <- &recvResult{
			data: data,
			err:  err,
		}
		if err != nil {
			return
		}
	}
}

// Send sends data over the session
func (ns *WebsocketSession) Send(data []byte) error {
	err := ns.conn.WriteMessage(websocket.BinaryMessage, data)
	if err != nil {
		return err
	}
	return nil
}

// Recv receives data via the session
func (ns *WebsocketSession) Recv(timeout time.Duration) ([]byte, error) {
	select {
	case rr := <-ns.recvChan:
		return rr.data, rr.err
	case <-time.After(timeout):
		return nil, netceptor.ErrTimeout
	}
}

// Close closes the session
func (ns *WebsocketSession) Close() error {
	if ns.closeChan != nil {
		ns.closeChanCloser.Do(func() {
			close(ns.closeChan)
			ns.closeChan = nil
		})
	}
	return ns.conn.Close()
}

// **************************************************************************
// Command line
// **************************************************************************

// WebsocketListenerCfg is the cmdline configuration object for a websocket listener
type WebsocketListenerCfg struct {
	BindAddr string             `description:"Local address to bind to" default:"0.0.0.0"`
	Port     int                `description:"Local TCP port to run http server on" barevalue:"yes" required:"yes"`
	TLS      string             `description:"Name of TLS server config"`
	Cost     float64            `description:"Connection cost (weight)" default:"1.0"`
	NodeCost map[string]float64 `description:"Per-node costs"`
}

// Prepare verifies the parameters are correct
func (cfg WebsocketListenerCfg) Prepare() error {
	if cfg.Cost <= 0.0 {
		return fmt.Errorf("connection cost must be positive")
	}
	for node, cost := range cfg.NodeCost {
		if cost <= 0.0 {
			return fmt.Errorf("connection cost must be positive for %s", node)
		}
	}
	return nil
}

// Run runs the action
func (cfg WebsocketListenerCfg) Run() error {
	address := fmt.Sprintf("%s:%d", cfg.BindAddr, cfg.Port)
	tlscfg, err := netceptor.MainInstance.GetServerTLSConfig(cfg.TLS)
	if err != nil {
		return err
	}
	b, err := NewWebsocketListener(address, tlscfg)
	if err != nil {
		logger.Error("Error creating listener %s: %s\n", address, err)
		return err
	}
	err = netceptor.MainInstance.AddBackend(b, cfg.Cost, cfg.NodeCost)
	if err != nil {
		return err
	}
	return nil
}

// WebsocketDialerCfg is the cmdline configuration object for a Websocket listener
type WebsocketDialerCfg struct {
	Address     string  `description:"URL to connect to" barevalue:"yes" required:"yes"`
	Redial      bool    `description:"Keep redialing on lost connection" default:"true"`
	ExtraHeader string  `description:"Sends extra HTTP header on initial connection"`
	TLS         string  `description:"Name of TLS client config"`
	Cost        float64 `description:"Connection cost (weight)" default:"1.0"`
}

// Prepare verifies that we are reasonably ready to go
func (cfg WebsocketDialerCfg) Prepare() error {
	if cfg.Cost <= 0.0 {
		return fmt.Errorf("connection cost must be positive")
	}
	_, err := url.Parse(cfg.Address)
	if err != nil {
		return fmt.Errorf("address %s is not a valid URL: %s", cfg.Address, err)
	}
	if cfg.ExtraHeader != "" && !strings.Contains(cfg.ExtraHeader, ":") {
		return fmt.Errorf("extra header must be in the form key:value")
	}
	return nil
}

// Run runs the action
func (cfg WebsocketDialerCfg) Run() error {
	logger.Debug("Running Websocket peer connection %s\n", cfg.Address)
	u, err := url.Parse(cfg.Address)
	if err != nil {
		return err
	}
	tlsCfgName := cfg.TLS
	if u.Scheme == "wss" && tlsCfgName == "" {
		tlsCfgName = "default"
	}
	tlscfg, err := netceptor.MainInstance.GetClientTLSConfig(tlsCfgName, u.Hostname(), "dns")
	if err != nil {
		return err
	}
	b, err := NewWebsocketDialer(cfg.Address, tlscfg, cfg.ExtraHeader, cfg.Redial)
	if err != nil {
		logger.Error("Error creating peer %s: %s\n", cfg.Address, err)
		return err
	}
	err = netceptor.MainInstance.AddBackend(b, cfg.Cost, nil)
	if err != nil {
		return err
	}
	return nil
}

func init() {
	cmdline.GlobalInstance().AddConfigType("ws-listener", "Run an http server that accepts websocket connections", WebsocketListenerCfg{}, cmdline.Section(backendSection))
	cmdline.GlobalInstance().AddConfigType("ws-peer", "Connect outbound to a websocket peer", WebsocketDialerCfg{}, cmdline.Section(backendSection))
}

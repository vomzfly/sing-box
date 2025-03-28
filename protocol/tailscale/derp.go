package tailscale

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/common/listener"
	"github.com/sagernet/sing-box/common/tls"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	F "github.com/sagernet/sing/common/format"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	aTLS "github.com/sagernet/sing/common/tls"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/filemanager"
	"github.com/sagernet/tailscale/derp"
	"github.com/sagernet/tailscale/derp/derphttp"
	"github.com/sagernet/tailscale/net/netmon"
	"github.com/sagernet/tailscale/net/stun"
	"github.com/sagernet/tailscale/net/wsconn"
	"github.com/sagernet/tailscale/tsweb"
	"github.com/sagernet/tailscale/types/key"

	"github.com/coder/websocket"
	"github.com/go-chi/render"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func RegisterDERPInbound(registry *inbound.Registry) {
	inbound.Register[option.DERPInboundOptions](registry, C.TypeDERP, NewDERPInbound)
}

type DERPInbound struct {
	inbound.Adapter
	ctx             context.Context
	logger          logger.ContextLogger
	listener        *listener.Listener
	stunListener    *listener.Listener
	dialer          N.Dialer
	tlsConfig       tls.ServerConfig
	server          *derp.Server
	configPath      string
	verifyClientURL []string
	home            string
	meshKey         string
	meshKeyPath     string
	meshWith        []option.DERPMeshOptions
}

func NewDERPInbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.DERPInboundOptions) (adapter.Inbound, error) {
	outboundDialer, err := dialer.NewWithOptions(dialer.Options{
		Context:          ctx,
		Options:          options.DialerOptions,
		RemoteIsDomain:   true,
		ResolverOnDetour: true,
		NewDialer:        true,
	})
	if err != nil {
		return nil, err
	}

	stunListenOptions := options.ListenOptions
	stunListenOptions.ListenPort = options.STUNPort

	if options.TLS == nil || !options.TLS.Enabled {
		return nil, E.New("TLS is required for DERP server")
	}
	tlsConfig, err := tls.NewServer(ctx, logger, common.PtrValueOrDefault(options.TLS))
	if err != nil {
		return nil, err
	}

	var configPath string
	if options.ConfigPath != "" {
		configPath = filemanager.BasePath(ctx, os.ExpandEnv(options.ConfigPath))
	} else if os.Getuid() == 0 {
		configPath = "/var/lib/derper/derper.key"
	} else {
		return nil, E.New("missing config_path")
	}

	if options.MeshPSK != "" {
		err = checkMeshKey(options.MeshPSK)
		if err != nil {
			return nil, E.Cause(err, "invalid mesh_psk")
		}
	}

	return &DERPInbound{
		Adapter: inbound.NewAdapter(C.TypeDERP, tag),
		ctx:     ctx,
		logger:  logger,
		listener: listener.New(listener.Options{
			Context: ctx,
			Logger:  logger,
			Network: []string{N.NetworkTCP},
			Listen:  options.ListenOptions,
		}),
		stunListener: listener.New(listener.Options{
			Context: ctx,
			Logger:  logger,
			Network: []string{N.NetworkTCP},
			Listen:  stunListenOptions,
		}),
		dialer:          outboundDialer,
		tlsConfig:       tlsConfig,
		configPath:      configPath,
		verifyClientURL: options.VerifyClientURL,
		meshKey:         options.MeshPSK,
		meshKeyPath:     options.MeshPSKFile,
		meshWith:        options.MeshWith,
	}, nil
}

func (d *DERPInbound) Start(stage adapter.StartStage) error {
	switch stage {
	case adapter.StartStateStart:
		config, err := readDERPConfig(d.configPath)
		if err != nil {
			return err
		}

		server := derp.NewServer(config.PrivateKey, func(format string, args ...any) {
			d.logger.Debug(fmt.Sprintf(format, args...))
		})
		server.SetVerifyClientHTTPClient(&http.Client{
			Transport: &http.Transport{
				ForceAttemptHTTP2: true,
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return d.dialer.DialContext(ctx, network, M.ParseSocksaddr(addr))
				},
			},
		})
		server.SetVerifyClientURL(d.verifyClientURL)

		if d.meshKey != "" {
			server.SetMeshKey(d.meshKey)
		} else if d.meshKeyPath != "" {
			var meshKeyContent []byte
			meshKeyContent, err = os.ReadFile(d.meshKeyPath)
			if err != nil {
				return err
			}
			err = checkMeshKey(string(meshKeyContent))
			if err != nil {
				return E.Cause(err, "invalid mesh_psk_path file")
			}
			server.SetMeshKey(string(meshKeyContent))
		}
		d.server = server

		derpMux := http.NewServeMux()
		derpHandler := derphttp.Handler(server)
		derpHandler = addWebSocketSupport(server, derpHandler)
		derpMux.Handle("/derp", derpHandler)

		homeHandler, ok := getHomeHandler(d.home)
		if !ok {
			return E.New("invalid home value: ", d.home)
		}

		derpMux.HandleFunc("/derp/probe", derphttp.ProbeHandler)
		derpMux.HandleFunc("/derp/latency-check", derphttp.ProbeHandler)
		derpMux.HandleFunc("/bootstrap-dns", tsweb.BrowserHeaderHandlerFunc(handleBootstrapDNS(d.ctx, d.dialer.(dialer.ResolveDialer).QueryOptions())))
		derpMux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tsweb.AddBrowserHeaders(w)
			homeHandler.ServeHTTP(w, r)
		}))
		derpMux.Handle("/robots.txt", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tsweb.AddBrowserHeaders(w)
			io.WriteString(w, "User-agent: *\nDisallow: /\n")
		}))
		derpMux.Handle("/generate_204", http.HandlerFunc(derphttp.ServeNoContent))

		err = d.tlsConfig.Start()
		if err != nil {
			return err
		}

		tcpListener, err := d.listener.ListenTCP()
		if err != nil {
			return err
		}
		if len(d.tlsConfig.NextProtos()) == 0 {
			d.tlsConfig.SetNextProtos([]string{http2.NextProtoTLS, "http/1.1"})
		} else if !common.Contains(d.tlsConfig.NextProtos(), http2.NextProtoTLS) {
			d.tlsConfig.SetNextProtos(append([]string{http2.NextProtoTLS}, d.tlsConfig.NextProtos()...))
		}
		tcpListener = aTLS.NewListener(tcpListener, d.tlsConfig)
		httpServer := &http.Server{
			Handler: h2c.NewHandler(derpMux, &http2.Server{}),
		}
		go httpServer.Serve(tcpListener)

		if d.stunListener.ListenOptions().ListenPort != 0 {
			packetConn, err := d.stunListener.ListenUDP()
			if err != nil {
				return err
			}
			go d.loopSTUN(packetConn.(*net.UDPConn))
		}
	case adapter.StartStatePostStart:
		if len(d.meshWith) > 0 {
			if !d.server.HasMeshKey() {
				return E.New("missing mesh psk")
			}
			for _, options := range d.meshWith {
				err := d.startMeshWithHost(d.server, options)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func checkMeshKey(meshKey string) error {
	checkRegex, err := regexp.Compile(`^[0-9a-f]{64}$`)
	if err != nil {
		return err
	}
	if !checkRegex.MatchString(meshKey) {
		return E.New("key must contain exactly 64 hex digits")
	}
	return nil
}

func (d *DERPInbound) startMeshWithHost(derpServer *derp.Server, server option.DERPMeshOptions) error {
	var hostname string
	if server.Hostname != "" {
		hostname = server.Hostname
	} else {
		hostname = server.Server
	}
	outboundDialer, err := dialer.NewWithOptions(dialer.Options{
		Context: d.ctx,
		Options: server.DialerOptions,
	})
	if err != nil {
		return err
	}
	var stdConfig *tls.STDConfig
	if server.TLS != nil && server.TLS.Enabled {
		tlsConfig, err := tls.NewClient(d.ctx, hostname, common.PtrValueOrDefault(server.TLS))
		if err != nil {
			return err
		}
		stdConfig, err = tlsConfig.Config()
		if err != nil {
			return err
		}
	}
	logf := func(format string, args ...any) {
		d.logger.Debug(F.ToString("mesh(", hostname, "): ", fmt.Sprintf(format, args...)))
	}
	meshClient, err := derphttp.NewClient(derpServer.PrivateKey(), "https://"+server.Build().String()+"/derp", logf, netmon.NewStatic())
	if err != nil {
		return err
	}
	meshClient.TLSConfig = stdConfig
	meshClient.MeshKey = derpServer.MeshKey()
	meshClient.WatchConnectionChanges = true
	meshClient.SetURLDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return outboundDialer.DialContext(ctx, network, M.ParseSocksaddr(addr))
	})
	add := func(m derp.PeerPresentMessage) { derpServer.AddPacketForwarder(m.Key, meshClient) }
	remove := func(m derp.PeerGoneMessage) { derpServer.RemovePacketForwarder(m.Peer, meshClient) }
	go meshClient.RunWatchConnectionLoop(context.Background(), derpServer.PublicKey(), logf, add, remove)
	return nil
}

func (d *DERPInbound) Close() error {
	return common.Close(
		common.PtrOrNil(d.listener),
		common.PtrOrNil(d.stunListener),
		d.tlsConfig,
	)
}

var homePage = `
<h1>DERP</h1>
<p>
  This is a <a href="https://tailscale.com/">Tailscale</a> DERP server.
</p>

<p>
  It provides STUN, interactive connectivity establishment, and relaying of end-to-end encrypted traffic
  for Tailscale clients.
</p>

<p>
  Documentation:
</p>

<ul>

<li><a href="https://tailscale.com/kb/1232/derp-servers">About DERP</a></li>
<li><a href="https://pkg.go.dev/tailscale.com/derp">Protocol & Go docs</a></li>
<li><a href="https://github.com/tailscale/tailscale/tree/main/cmd/derper#derp">How to run a DERP server</a></li>

</body>
</html>
`

func getHomeHandler(val string) (_ http.Handler, ok bool) {
	if val == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(200)
			w.Write([]byte(homePage))
		}), true
	}
	if val == "blank" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(200)
		}), true
	}
	if strings.HasPrefix(val, "http://") || strings.HasPrefix(val, "https://") {
		return http.RedirectHandler(val, http.StatusFound), true
	}
	return nil, false
}

func addWebSocketSupport(s *derp.Server, base http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := strings.ToLower(r.Header.Get("Upgrade"))

		// Very early versions of Tailscale set "Upgrade: WebSocket" but didn't actually
		// speak WebSockets (they still assumed DERP's binary framing). So to distinguish
		// clients that actually want WebSockets, look for an explicit "derp" subprotocol.
		if up != "websocket" || !strings.Contains(r.Header.Get("Sec-Websocket-Protocol"), "derp") {
			base.ServeHTTP(w, r)
			return
		}

		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols:   []string{"derp"},
			OriginPatterns: []string{"*"},
			// Disable compression because we transmit WireGuard messages that
			// are not compressible.
			// Additionally, Safari has a broken implementation of compression
			// (see https://github.com/nhooyr/websocket/issues/218) that makes
			// enabling it actively harmful.
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusInternalError, "closing")
		if c.Subprotocol() != "derp" {
			c.Close(websocket.StatusPolicyViolation, "client must speak the derp subprotocol")
			return
		}
		wc := wsconn.NetConn(r.Context(), c, websocket.MessageBinary, r.RemoteAddr)
		brw := bufio.NewReadWriter(bufio.NewReader(wc), bufio.NewWriter(wc))
		s.Accept(r.Context(), wc, brw, r.RemoteAddr)
	})
}

func handleBootstrapDNS(ctx context.Context, queryOptions adapter.DNSQueryOptions) http.HandlerFunc {
	dnsRouter := service.FromContext[adapter.DNSRouter](ctx)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Connection", "close")
		if queryDomain := r.URL.Query().Get("q"); queryDomain != "" {
			addresses, err := dnsRouter.Lookup(ctx, queryDomain, queryOptions)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			render.JSON(w, r, render.M{
				queryDomain: addresses,
			})
			return
		}
		w.Write([]byte("{}"))
	}
}

func (d *DERPInbound) loopSTUN(packetConn *net.UDPConn) {
	var buffer [64 << 10]byte
	var (
		n        int
		addrPort netip.AddrPort
		err      error
	)
	for {
		n, addrPort, err = packetConn.ReadFromUDPAddrPort(buffer[:])
		if err != nil {
			if E.IsClosedOrCanceled(err) {
				return
			}
			time.Sleep(time.Second)
			continue
		}
		pkt := buffer[:n]
		if !stun.Is(pkt) {
			continue
		}
		txid, err := stun.ParseBindingRequest(pkt)
		if err != nil {
			continue
		}
		packetConn.WriteToUDPAddrPort(stun.Response(txid, addrPort), addrPort)
	}
}

type derpConfig struct {
	PrivateKey key.NodePrivate
}

func readDERPConfig(path string) (*derpConfig, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return writeNewDERPConfig(path)
		}
		return nil, err
	}
	var config derpConfig
	err = json.Unmarshal(content, &config)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

func writeNewDERPConfig(path string) (*derpConfig, error) {
	newKey := key.NewNode()
	err := os.MkdirAll(filepath.Dir(path), 0o777)
	if err != nil {
		return nil, err
	}
	config := derpConfig{
		PrivateKey: newKey,
	}
	content, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	err = os.WriteFile(path, content, 0o644)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

package tailscale

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-dns"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/tailscale/ipn"
	nDNS "github.com/sagernet/tailscale/net/dns"
	"github.com/sagernet/tailscale/wgengine/router"
	"github.com/sagernet/tailscale/wgengine/wgcfg"

	mDNS "github.com/miekg/dns"
	"go4.org/netipx"
)

func init() {
	dns.RegisterTransport([]string{"tailscale"}, func(options dns.TransportOptions) (dns.Transport, error) {
		return NewDNSTransport(options)
	})
}

type DNSTransport struct {
	endpointTag     string
	options         dns.TransportOptions
	network         adapter.NetworkManager
	endpointManager adapter.EndpointManager
	endpoint        *Endpoint
	rawConfig       *wgcfg.Config
	rawDNSConfig    **nDNS.Config
	routePrefixes   []netip.Prefix
	dnsClient       *dns.Client
	routes          map[string][]dns.Transport
	hosts           map[string][]netip.Addr
}

func NewDNSTransport(options dns.TransportOptions) (dns.Transport, error) {
	linkURL, err := url.Parse(options.Address)
	if err != nil {
		return nil, err
	}
	if linkURL.Host == "" {
		return nil, E.New("missing tailscale outbound tag")
	}
	return &DNSTransport{
		endpointTag:     linkURL.Host,
		options:         options,
		network:         service.FromContext[adapter.NetworkManager](options.Context),
		endpointManager: service.FromContext[adapter.EndpointManager](options.Context),
	}, nil
}

func (t *DNSTransport) Name() string {
	return t.options.Name
}

func (t *DNSTransport) Start() error {
	rawOutbound, loaded := t.endpointManager.Get(t.endpointTag)
	if !loaded {
		return E.New("endpoint not found: ", t.endpointTag)
	}
	tsOutbound, isTailscale := rawOutbound.(*Endpoint)
	if !isTailscale {
		return E.New("endpoint is not tailscale: ", t.endpointTag)
	}
	t.endpoint = tsOutbound
	go tsOutbound.server.ExportLocalBackend().WatchNotifications(t.options.Context, ipn.NotifyInitialState, nil, func(roNotify *ipn.Notify) (keepGoing bool) {
		if roNotify.State != nil {
			if *roNotify.State == ipn.Running {
				err := t.updateDNSServers()
				if err == nil {
					t.options.Logger.Info("initialized")
				}
				return err != nil
			}
		}
		if roNotify.LoginFinished != nil {
			err := t.updateDNSServers()
			if err == nil {
				t.options.Logger.Info("initialized")
			}
			return err != nil
		}
		return true
	})
	return nil
}

func (t *DNSTransport) Reset() {
}

func (t *DNSTransport) updateDNSServers() error {
	config, dnsConfig, routeConfig := t.endpoint.server.ExportLocalBackend().ExportConfig()
	if config == nil || dnsConfig == nil {
		return os.ErrInvalid
	}
	t.routePrefixes = buildRoutePrefixes(routeConfig)
	directDialerOnce := sync.OnceValue(func() N.Dialer {
		directDialer := common.Must1(dialer.NewDefault(t.network, option.DialerOptions{}))
		return &DNSDialer{transport: t, fallbackDialer: directDialer}
	})
	routes := make(map[string][]dns.Transport)
	for domain, resolvers := range dnsConfig.Routes {
		var myResolvers []dns.Transport
		for _, resolver := range resolvers {
			myDialer := directDialerOnce()
			if len(resolver.BootstrapResolution) > 0 {
				bootstrapTransport := common.Must1(dns.CreateTransport(dns.TransportOptions{
					Context: t.options.Context,
					Logger:  t.options.Logger,
					Dialer:  directDialerOnce(),
					Address: resolver.BootstrapResolution[0].String(),
				}))
				myDialer = dns.NewDialerWrapper(myDialer, t.dnsClient, bootstrapTransport, dns.DomainStrategyPreferIPv4, 0)
			}
			transport, err := dns.CreateTransport(dns.TransportOptions{
				Context: t.options.Context,
				Logger:  t.options.Logger,
				Dialer:  myDialer,
				Address: resolver.Addr,
			})
			if err != nil {
				return E.Cause(err, "parse resolver: ", resolver.Addr)
			}
			myResolvers = append(myResolvers, transport)
		}
		routes[domain.WithTrailingDot()] = myResolvers
	}
	hosts := make(map[string][]netip.Addr)
	for domain, addresses := range dnsConfig.Hosts {
		hosts[domain.WithTrailingDot()] = addresses
	}
	t.routes = routes
	t.hosts = hosts
	return nil
}

func buildRoutePrefixes(routeConfig *router.Config) []netip.Prefix {
	var builder netipx.IPSetBuilder
	for _, localAddr := range routeConfig.LocalAddrs {
		builder.AddPrefix(localAddr)
	}
	for _, route := range routeConfig.Routes {
		builder.AddPrefix(route)
	}
	for _, route := range routeConfig.LocalRoutes {
		builder.AddPrefix(route)
	}
	for _, route := range routeConfig.SubnetRoutes {
		builder.AddPrefix(route)
	}
	ipSet, err := builder.IPSet()
	if err != nil {
		return nil
	}
	return ipSet.Prefixes()
}

func (t *DNSTransport) Close() error {
	return nil
}

func (t *DNSTransport) Raw() bool {
	return true
}

func (t *DNSTransport) Exchange(ctx context.Context, message *mDNS.Msg) (*mDNS.Msg, error) {
	if len(message.Question) != 1 {
		return nil, os.ErrInvalid
	}
	question := message.Question[0]
	addresses, hostsLoaded := t.hosts[question.Name]
	if hostsLoaded {
		switch question.Qtype {
		case mDNS.TypeA:
			addresses4 := common.Filter(addresses, func(addr netip.Addr) bool {
				return addr.Is4()
			})
			if len(addresses4) > 0 {
				return dns.FixedResponse(message.Id, question, addresses4, dns.DefaultTTL), nil
			}
		case mDNS.TypeAAAA:
			addresses6 := common.Filter(addresses, func(addr netip.Addr) bool {
				return addr.Is6()
			})
			if len(addresses6) > 0 {
				return dns.FixedResponse(message.Id, question, addresses6, dns.DefaultTTL), nil
			}
		}
	}
	for domainSuffix, transports := range t.routes {
		if strings.HasSuffix(question.Name, domainSuffix) {
			if len(transports) == 0 {
				return &mDNS.Msg{
					MsgHdr: mDNS.MsgHdr{
						Id:       message.Id,
						Rcode:    mDNS.RcodeNameError,
						Response: true,
					},
					Question: []mDNS.Question{question},
				}, nil
			}
			return transports[0].Exchange(ctx, message)
		}
	}
	return nil, dns.RCodeNameError
}

func (t *DNSTransport) Lookup(ctx context.Context, domain string, strategy dns.DomainStrategy) ([]netip.Addr, error) {
	return nil, os.ErrInvalid
}

type DNSDialer struct {
	transport      *DNSTransport
	fallbackDialer N.Dialer
}

func (d *DNSDialer) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if destination.IsFqdn() {
		panic("invalid request here")
	}
	for _, prefix := range d.transport.routePrefixes {
		if prefix.Contains(destination.Addr) {
			return d.transport.endpoint.DialContext(ctx, network, destination)
		}
	}
	return d.fallbackDialer.DialContext(ctx, network, destination)
}

func (d *DNSDialer) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	if destination.IsFqdn() {
		panic("invalid request here")
	}
	for _, prefix := range d.transport.routePrefixes {
		if prefix.Contains(destination.Addr) {
			return d.transport.endpoint.ListenPacket(ctx, destination)
		}
	}
	return d.fallbackDialer.ListenPacket(ctx, destination)
}

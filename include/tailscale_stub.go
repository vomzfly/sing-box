//go:build !with_tailscale

package include

import (
	"context"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-dns"
	E "github.com/sagernet/sing/common/exceptions"
)

func init() {
	dns.RegisterTransport([]string{C.TypeTailscale}, func(options dns.TransportOptions) (dns.Transport, error) {
		return nil, E.New(`Tailscale is not included in this build, rebuild with -tags with_tailscale`)
	})
}

func registerTailscaleEndpoint(registry *endpoint.Registry) {
	outbound.Register[option.TailscaleEndpointOptions](registry, C.TypeTailscale, func(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.TailscaleEndpointOptions) (adapter.Outbound, error) {
		return nil, E.New(`Tailscale is not included in this build, rebuild with -tags with_tailscale`)
	})
}

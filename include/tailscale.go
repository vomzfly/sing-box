//go:build with_tailscale

package include

import (
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/protocol/tailscale"
)

func registerTailscaleInbound(registry *inbound.Registry) {
	tailscale.RegisterDERPInbound(registry)
}

func registerTailscaleEndpoint(registry *endpoint.Registry) {
	tailscale.RegisterEndpoint(registry)
}

func registerTailscaleTransport(registry *dns.TransportRegistry) {
	tailscale.RegistryTransport(registry)
}

package option

import (
	"net/netip"

	"github.com/sagernet/sing/common/json/badoption"
)

type TailscaleEndpointOptions struct {
	DialerOptions
	StateDirectory         string           `json:"state_directory,omitempty"`
	AuthKey                string           `json:"auth_key,omitempty"`
	ControlURL             string           `json:"control_url,omitempty"`
	Ephemeral              bool             `json:"ephemeral,omitempty"`
	Hostname               string           `json:"hostname,omitempty"`
	ExitNode               string           `json:"exit_node,omitempty"`
	ExitNodeAllowLANAccess bool             `json:"exit_node_allow_lan_access,omitempty"`
	AdvertiseRoutes        []netip.Prefix   `json:"advertise_routes,omitempty"`
	AdvertiseExitNode      bool             `json:"advertise_exit_node,omitempty"`
	UDPTimeout             UDPTimeoutCompat `json:"udp_timeout,omitempty"`
}

type TailscaleDNSServerOptions struct {
	Endpoint               string `json:"endpoint,omitempty"`
	AcceptDefaultResolvers bool   `json:"accept_default_resolvers,omitempty"`
}

type DERPInboundOptions struct {
	ListenOptions
	STUNPort uint16 `json:"stun_port,omitempty"`
	InboundTLSOptionsContainer
	ConfigPath      string                     `json:"config_path,omitempty"`
	VerifyClientURL badoption.Listable[string] `json:"verify_client_url,omitempty"`
	MeshWith        []DERPMeshOptions          `json:"mesh_with,omitempty"`
	MeshPSK         string                     `json:"mesh_psk,omitempty"`
	MeshPSKFile     string                     `json:"mesh_psk_file,omitempty"`
	DialerOptions
}

type DERPMeshOptions struct {
	ServerOptions
	DialerOptions
	OutboundTLSOptionsContainer
	Hostname string `json:"hostname,omitempty"`
}

package network

import (
	"fmt"
	"net"

	whereaboutsk8s "github.com/k8snetworkplumbingwg/whereabouts/pkg/storage/kubernetes"
)

// WhereaboutsIPPoolName validates and normalizes the CIDR, then delegates naming
// to Whereabouts using the default network and node identifiers.
func WhereaboutsIPPoolName(cidr string) (string, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}
	return whereaboutsk8s.IPPoolName(whereaboutsk8s.PoolIdentifier{
		IpRange: network.String(),
	}), nil
}

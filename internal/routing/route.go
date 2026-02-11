// Package routing provides selective routing based on destination patterns
package routing

// Route represents a routing destination
type Route string

const (
	// RouteProxy routes traffic through the EC2 proxy
	RouteProxy Route = "proxy"
	// RouteDirect routes traffic directly from the host
	RouteDirect Route = "direct"
	// RouteVMDirect routes traffic through VM's NAT (VM mode only)
	RouteVMDirect Route = "vm-direct"
)

// IsValid checks if the route is a valid route type
func (r Route) IsValid() bool {
	switch r {
	case RouteProxy, RouteDirect, RouteVMDirect:
		return true
	default:
		return false
	}
}

// String returns the string representation of the route
func (r Route) String() string {
	return string(r)
}

// Package platform applies and reverts OS-specific proxy routing and DNS
// configuration required by the mole client.
//
// Each platform (linux.go, darwin.go, windows.go, other.go) provides a
// Manager that implements Apply() and Revert(). The Setup struct carries the
// parameters that the caller (cmd/client) passes to NewManager.
package platform

// Setup holds the runtime parameters that cmd/client passes to the platform
// Manager. All fields are read-only after NewManager returns.
type Setup struct {
	// ServerIPs is the list of proxy server host addresses. Traffic destined
	// for these IPs is exempted from the iptables REDIRECT rules so that the
	// proxy connection itself is never intercepted.
	ServerIPs []string

	// ActiveServerHost is the hostname or IP of the currently-selected
	// SOCKS5 proxy server.
	ActiveServerHost string

	// ActiveServerSocks5Port is the SOCKS5 port on ActiveServerHost.
	ActiveServerSocks5Port int

	// TransparentProxyPort is the local port where the transparent TCP proxy
	// listens. On Linux, iptables REDIRECT rules steer intercepted traffic
	// here. On other platforms this is used for display/logging only.
	TransparentProxyPort int

	// DNSListenAddr is the local address (host:port) where the DNS proxy
	// listens. On Linux, resolv.conf is overridden to point here.
	DNSListenAddr string
}

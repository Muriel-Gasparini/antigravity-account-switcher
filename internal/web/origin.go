package web

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// isManagementPath reports whether the request targets the dashboard API or
// the OAuth onboarding endpoints. Proxy traffic is deliberately excluded: it
// comes from the Antigravity process rather than from a browser page.
func isManagementPath(p string) bool {
	return strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/oauth/")
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// requestFromTrustedOrigin decides whether a state-changing request may run.
//
// A browser always attaches Origin to cross-origin POST/PUT/DELETE, so a
// present Origin must be this server's own origin. Without it the request
// falls back to Fetch Metadata and is refused only when the browser says it is
// cross-site. Requests carrying neither header (curl, scripts) are allowed.
func (s *Server) requestFromTrustedOrigin(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		return s.isOwnOrigin(r, origin)
	}
	return !strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site")
}

// isOwnOrigin compares origin against the address this server is listening
// on, not against the Host header, which a DNS-rebinding page controls. The
// loopback names localhost, 127.0.0.1 and [::1] are interchangeable for the
// same port, as is the local IP the connection arrived on.
func (s *Server) isOwnOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") {
		return false
	}

	wantScheme := "http"
	if r.TLS != nil {
		wantScheme = "https"
	}
	if u.Scheme != wantScheme {
		return false
	}

	localHost, localPort := s.localEndpoint(r)

	gotPort := u.Port()
	if gotPort == "" {
		gotPort = "80"
		if wantScheme == "https" {
			gotPort = "443"
		}
	}
	if gotPort != localPort {
		return false
	}

	host := strings.ToLower(u.Hostname())
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return localHost != "" && host == strings.ToLower(localHost)
}

// localEndpoint returns the host and port the request's connection reached.
// It falls back to the configured port when the request did not come through
// a net/http server (for example a recorder in a unit test).
func (s *Server) localEndpoint(r *http.Request) (host, port string) {
	if addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && addr != nil {
		if h, p, err := net.SplitHostPort(addr.String()); err == nil {
			return h, p
		}
	}
	return "", strconv.Itoa(s.Port())
}

func writeForbiddenOrigin(w http.ResponseWriter) {
	writeErrorJSON(w, http.StatusForbidden, "cross-origin request rejected", nil)
}

package preview

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/wins/jaz/backend/internal/httpapi"
)

type createProxyRequest struct {
	URL string `json:"url"`
}

func (h *Handler) createProxy(w http.ResponseWriter, r *http.Request) {
	var input createProxyRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, fmt.Errorf("read proxy request: %w", err))
		return
	}
	target, err := parseProxyTarget(input.URL)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, err)
		return
	}
	origin := &url.URL{Scheme: target.Scheme, Host: target.Host, Path: "/"}
	h.register(w, r, target, previewEntry{
		baseURL: origin.String(),
		serve: func(w http.ResponseWriter, r *http.Request) {
			serveProxy(w, r, origin)
		},
	})
}

func parseProxyTarget(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("preview proxy only supports http and https")
	}
	if u.User != nil || u.Host == "" {
		return nil, fmt.Errorf("proxy URL must include an origin")
	}
	if !loopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("preview proxy only supports server-local loopback URLs")
	}
	if u.Path == "" {
		u.Path = "/"
	}
	if u.Hostname() == "0.0.0.0" {
		if port := u.Port(); port != "" {
			u.Host = net.JoinHostPort("127.0.0.1", port)
		} else {
			u.Host = "127.0.0.1"
		}
	}
	return u, nil
}

func serveProxy(w http.ResponseWriter, r *http.Request, target *url.URL) {
	escapedPath := r.URL.EscapedPath()
	path, err := url.PathUnescape(escapedPath)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, err)
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			out := proxyRequest.Out
			out.URL.Scheme = target.Scheme
			out.URL.Host = target.Host
			out.URL.Path = path
			if strings.Contains(escapedPath, "%") {
				out.URL.RawPath = escapedPath
			}
			out.URL.RawQuery = r.URL.RawQuery
			out.Host = target.Host
			stripCredentials(out.Header)
		},
		ModifyResponse: func(response *http.Response) error {
			rewriteLocation(response, target, httpapi.RequestBaseURL(r))
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			httpapi.WriteError(w, http.StatusBadGateway, err)
		},
	}
	proxy.ServeHTTP(w, r)
}

func stripCredentials(header http.Header) {
	for name := range header {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "proxy-authorization" || lower == "cookie" || lower == "origin" || lower == "referer" || strings.HasPrefix(lower, "x-jaz-") {
			header.Del(name)
		}
	}
}

func rewriteLocation(response *http.Response, target *url.URL, publicOrigin string) {
	raw := response.Header.Get("Location")
	location, err := url.Parse(raw)
	if err != nil || location.Host == "" || !strings.EqualFold(location.Host, target.Host) || (location.Scheme != "" && !strings.EqualFold(location.Scheme, target.Scheme)) {
		return
	}
	public, err := url.Parse(publicOrigin)
	if err != nil || public.Scheme == "" || public.Host == "" {
		return
	}
	location.Scheme = public.Scheme
	location.Host = public.Host
	response.Header.Set("Location", location.String())
}

func loopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" || host == "0.0.0.0" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

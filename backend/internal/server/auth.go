package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/wins/jaz/backend/internal/deviceauth"
	"github.com/wins/jaz/backend/internal/httpapi"
	"github.com/wins/jaz/backend/internal/serverconfig"
	"github.com/wins/jaz/backend/internal/sessioncontext"
)

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := s.AuthKey
		if key == "" || r.URL.Path == "/health" || publicOAuthCallback(r) {
			next.ServeHTTP(w, r.WithContext(contextWithClientInfo(r, deviceauth.Principal{})))
			return
		}
		if publicDeviceRequest(r) {
			identity := s.DeviceProxy.Identify(r)
			principal := deviceauth.Principal{User: identity.User, Owner: identity.Owner}
			next.ServeHTTP(w, r.WithContext(contextWithClientInfo(r, principal)))
			return
		}
		if s.Devices != nil && queryAuthAllowed(r) && r.URL.Query().Get("ticket") != "" {
			principal, err := s.Devices.AuthenticateResourceTicket(r.URL.Query().Get("ticket"))
			if err != nil {
				writeAuthError(w, http.StatusUnauthorized, "unauthorized", err)
				return
			}
			s.serveDevice(w, r, principal, next)
			return
		}
		token := requestAuthKey(r)
		identity := s.DeviceProxy.Identify(r)
		if s.Devices != nil && r.Method == http.MethodPost && r.URL.Path == "/v1/devices/register" && strings.HasPrefix(token, "invite_") {
			principal := deviceauth.Principal{Kind: deviceauth.PrincipalEnrollment, EnrollmentToken: token, User: identity.User}
			next.ServeHTTP(w, r.WithContext(contextWithClientInfo(r, principal)))
			return
		}
		if s.Devices != nil {
			info := httpapi.RequestInfoFrom(r)
			principal, err := s.Devices.Authenticate(token, requestProof(r), identity, deviceauth.SeenInfo{IP: info.IP, UserAgent: info.UserAgent})
			if err == nil {
				s.serveDevice(w, r, principal, next)
				return
			}
			// Kept apart from unauthorized: the request bypassed the auth proxy,
			// which says nothing about the device token.
			if errors.Is(err, deviceauth.ErrUserRequired) {
				writeAuthError(w, http.StatusUnauthorized, "user_required", err)
				return
			}
			// Distinct from unauthorized so clients keep a token whose proof was
			// merely stale or replayed instead of discarding it and re-pairing.
			if errors.Is(err, deviceauth.ErrProofInvalid) {
				writeAuthError(w, http.StatusUnauthorized, "invalid_dpop_proof", err)
				return
			}
			if errors.Is(err, deviceauth.ErrApprovalRequired) {
				writeAuthError(w, http.StatusForbidden, "device_approval_required", err)
				return
			}
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(key)) == 1 {
			if s.Devices == nil || (trustedLocalRequest(r) && (rootKeyAllowed(r) || s.rootKeyHasFullAccess())) {
				principal := deviceauth.Principal{Kind: deviceauth.PrincipalRoot}
				next.ServeHTTP(w, r.WithContext(contextWithClientInfo(r, principal)))
				return
			}
			writeAuthError(w, http.StatusForbidden, "device_approval_required", deviceauth.ErrApprovalRequired)
			return
		}
		writeAuthError(w, http.StatusUnauthorized, "unauthorized", fmt.Errorf("missing or invalid backend API key"))
	})
}

const clientPlatformHeader = "X-Jaz-Client-Platform"

func contextWithClientInfo(r *http.Request, principal deviceauth.Principal) context.Context {
	ctx := r.Context()
	if principal != (deviceauth.Principal{}) {
		ctx = deviceauth.WithPrincipal(ctx, principal)
	}
	return sessioncontext.WithClientPlatform(ctx, requestClientPlatform(r))
}

func requestClientPlatform(r *http.Request) string {
	if platform := strings.TrimSpace(r.Header.Get(clientPlatformHeader)); platform != "" {
		switch platform {
		case "browser", "cli", "desktop", "mobile":
			return platform
		}
	}
	return "desktop"
}

func (s *Server) rootKeyHasFullAccess() bool {
	if s.Devices == nil {
		return true
	}
	count, err := s.Devices.ApprovedDeviceCount()
	return err == nil && count == 0
}

func rootKeyAllowed(r *http.Request) bool {
	return r.URL.Path == "/v1/devices" || strings.HasPrefix(r.URL.Path, "/v1/devices/") || internalMCPRequest(r)
}

func trustedLocalRequest(r *http.Request) bool {
	if !loopbackRequest(r) {
		return false
	}
	host := (&url.URL{Host: r.Host}).Hostname()
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return false
	}
	for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "CF-Connecting-IP"} {
		if r.Header.Get(name) != "" {
			return false
		}
	}
	return true
}

func (s *Server) serveDevice(w http.ResponseWriter, r *http.Request, principal deviceauth.Principal, next http.Handler) {
	ctx, release, err := s.Devices.Track(contextWithClientInfo(r, principal), principal)
	if err != nil {
		writeAuthError(w, http.StatusUnauthorized, "unauthorized", err)
		return
	}
	defer release()
	next.ServeHTTP(w, r.WithContext(ctx))
}

func publicDeviceRequest(r *http.Request) bool {
	if r.Method == http.MethodPost && r.URL.Path == "/v1/devices/pairing-requests" {
		return true
	}
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/devices/pairing-requests/") {
		rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/devices/pairing-requests/"), "/")
		return rest != "" && !strings.Contains(rest, "/")
	}
	return false
}

func publicOAuthCallback(r *http.Request) bool {
	return r.Method == http.MethodGet && (r.URL.Path == "/v1/connections/oauth/callback" || r.URL.Path == mcpOAuthCallbackPath)
}

func writeAuthError(w http.ResponseWriter, status int, code string, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error(), "code": code})
}

func (s *Server) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	principal, _ := deviceauth.PrincipalFromContext(r.Context())
	response := map[string]any{
		"ok":        true,
		"auth_kind": principal.Kind,
		"device_id": principal.DeviceID,
	}
	if principal.Kind == deviceauth.PrincipalDevice {
		ticket, err := s.Devices.IssueResourceTicket(requestAuthKey(r))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		response["resource_ticket"] = ticket
	}
	writeJSON(w, http.StatusOK, response)
}

func requestAuthKey(r *http.Request) string {
	if token := bearerToken(r.Header.Get("Authorization")); token != "" {
		return token
	}
	return webSocketProtocol(r, "jaz-auth.")
}

// requestProof reads the DPoP proof from its header or, since browsers cannot
// set headers on a WebSocket handshake, from a subprotocol.
func requestProof(r *http.Request) deviceauth.Proof {
	jwt := r.Header.Get(deviceauth.ProofHeader)
	if jwt == "" {
		jwt = webSocketProtocol(r, "jaz-dpop.")
	}
	return deviceauth.Proof{JWT: jwt, Method: r.Method, Path: r.URL.Path}
}

func webSocketProtocol(r *http.Request, prefix string) string {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return ""
	}
	for _, protocol := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(protocol), prefix); ok {
			return value
		}
	}
	return ""
}

func queryAuthAllowed(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if r.URL.Path == "/v1/bots/events" {
		return true
	}
	sessionPath := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	if sessionPath == r.URL.Path {
		return false
	}
	sessionRef, action, ok := strings.Cut(sessionPath, "/")
	if !ok || sessionRef == "" {
		return false
	}
	return action == "events" ||
		strings.HasPrefix(action, "attachments/") ||
		(action == "file" && rawSessionFileRequested(r))
}

func bearerToken(header string) string {
	fields := strings.Fields(header)
	if len(fields) == 2 && (strings.EqualFold(fields[0], "Bearer") || strings.EqualFold(fields[0], "DPoP")) {
		return fields[1]
	}
	return ""
}

func internalMCPRequest(r *http.Request) bool {
	switch r.URL.Path {
	case serverconfig.JazToolsMCPPath, serverconfig.JazToolsMCPCompatPath, serverconfig.JazmemMCPPath, serverconfig.MCPProxyPath:
		return loopbackRequest(r)
	default:
		return false
	}
}

func loopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

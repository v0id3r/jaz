package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/wins/jaz/backend/internal/deviceauth"
	"github.com/wins/jaz/backend/internal/httpapi"
	devicesapi "github.com/wins/jaz/backend/internal/httpapi/devices"
	"github.com/wins/jaz/backend/internal/serverconfig"
	"github.com/wins/jaz/backend/internal/sessioncontext"
)

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := s.AuthKey
		if key == "" || r.URL.Path == "/health" || publicDeviceRequest(r) || publicOAuthCallback(r) {
			next.ServeHTTP(w, r.WithContext(contextWithClientInfo(r, deviceauth.Principal{})))
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
		if s.Devices != nil && r.Method == http.MethodPost && r.URL.Path == "/v1/devices/register" && strings.HasPrefix(token, "invite_") {
			principal := deviceauth.Principal{Kind: deviceauth.PrincipalEnrollment, EnrollmentToken: token}
			next.ServeHTTP(w, r.WithContext(contextWithClientInfo(r, principal)))
			return
		}
		var deviceErr error
		var deviceToken string
		if s.Devices != nil {
			deviceToken = s.deviceToken(r)
			info := httpapi.RequestInfoFrom(r)
			principal, err := s.Devices.Authenticate(deviceToken, devicesapi.RequestProof(r), deviceauth.SeenInfo{IP: info.IP, UserAgent: info.UserAgent})
			if err == nil {
				s.serveDevice(w, r, principal, next)
				return
			}
			// Distinct from unauthorized so clients keep a token whose proof was
			// merely stale or replayed instead of discarding it and re-pairing.
			if errors.Is(err, deviceauth.ErrProofInvalid) {
				s.writeUnauthorized(w, "invalid_dpop_proof", "invalid_dpop_proof", err)
				return
			}
			if errors.Is(err, deviceauth.ErrApprovalRequired) {
				writeAuthError(w, http.StatusForbidden, "device_approval_required", err)
				return
			}
			deviceErr = err
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(key)) == 1 {
			if s.Devices == nil || (httpapi.TrustedLocalRequest(r) && (rootKeyAllowed(r) || s.rootKeyHasFullAccess())) {
				principal := deviceauth.Principal{Kind: deviceauth.PrincipalRoot}
				next.ServeHTTP(w, r.WithContext(contextWithClientInfo(r, principal)))
				return
			}
			writeAuthError(w, http.StatusForbidden, "device_approval_required", deviceauth.ErrApprovalRequired)
			return
		}
		// An expired access token only needs a new one from the token endpoint.
		if errors.Is(deviceErr, deviceauth.ErrTokenInvalid) {
			s.writeUnauthorized(w, "invalid_token", "invalid_token", deviceErr)
			return
		}
		challengeError := ""
		if token != "" || deviceToken != "" {
			challengeError = "invalid_token"
		}
		s.writeUnauthorized(w, "unauthorized", challengeError, fmt.Errorf("missing or invalid backend API key"))
	})
}

// deviceToken is the credential offered for device auth. In proof mode it is
// a DPoP-bound access token, which must not be accepted as a bearer token
// (RFC 9449 §7.2), nor a bearer token as one.
func (s *Server) deviceToken(r *http.Request) string {
	if s.Devices.RequireProof {
		return requestCredential(r, "DPoP")
	}
	return requestAuthKey(r)
}

// writeUnauthorized adds the DPoP challenge of RFC 9449 §7.1 in proof mode,
// with challengeError unless the request presented no credential; the code in
// the body stays what Jaz clients branch on.
func (s *Server) writeUnauthorized(w http.ResponseWriter, code, challengeError string, err error) {
	if s.Devices != nil && s.Devices.RequireProof {
		challenge := `DPoP algs="` + deviceauth.ProofAlgs + `"`
		if challengeError != "" {
			challenge += `, error="` + challengeError + `"`
		}
		w.Header().Set("WWW-Authenticate", challenge)
	}
	writeAuthError(w, http.StatusUnauthorized, code, err)
}

const clientPlatformHeader = "X-Jaz-Client-Platform"

func contextWithClientInfo(r *http.Request, principal deviceauth.Principal) context.Context {
	ctx := r.Context()
	if principal.Kind != "" {
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
	if r.Method == http.MethodPost && (r.URL.Path == "/v1/devices/pairing-requests" || r.URL.Path == "/v1/devices/token") {
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
		ticket, err := s.Devices.IssueResourceTicket(s.deviceToken(r))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		response["resource_ticket"] = ticket
	}
	writeJSON(w, http.StatusOK, response)
}

func requestAuthKey(r *http.Request) string {
	return requestCredential(r, "Bearer")
}

// requestCredential reads a credential presented under scheme, or as the
// subprotocol a browser WebSocket handshake carries it in.
func requestCredential(r *http.Request, scheme string) string {
	if fields := strings.Fields(r.Header.Get("Authorization")); len(fields) == 2 && strings.EqualFold(fields[0], scheme) {
		return fields[1]
	}
	return httpapi.WebSocketProtocol(r, "jaz-auth.")
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

func internalMCPRequest(r *http.Request) bool {
	switch r.URL.Path {
	case serverconfig.JazToolsMCPPath, serverconfig.JazToolsMCPCompatPath, serverconfig.JazmemMCPPath, serverconfig.MCPProxyPath:
		return httpapi.LoopbackRequest(r)
	default:
		return false
	}
}

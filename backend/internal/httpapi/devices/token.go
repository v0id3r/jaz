package devices

import (
	"errors"
	"mime"
	"net/http"

	"github.com/wins/jaz/backend/internal/deviceauth"
	"github.com/wins/jaz/backend/internal/httpapi"
)

// RequestProof reads the single DPoP proof of a request from its header or,
// since browsers cannot set headers on a WebSocket handshake, from a
// subprotocol. Only a request that reached loopback without a proxy vouches
// for its own origin.
func RequestProof(r *http.Request) deviceauth.Proof {
	proof := deviceauth.Proof{Method: r.Method, Path: r.URL.Path}
	switch values := r.Header.Values(deviceauth.ProofHeader); len(values) {
	case 0:
		proof.JWT = httpapi.WebSocketProtocol(r, "jaz-dpop.")
	case 1:
		proof.JWT = values[0]
	}
	if httpapi.TrustedLocalRequest(r) {
		proof.LocalOrigin = httpapi.RequestBaseURL(r)
	}
	return proof
}

// Token is the OAuth token endpoint of proof mode (RFC 6749 §3.2): a device
// authenticates with a private_key_jwt assertion (RFC 7523) and receives a
// DPoP-bound access token (RFC 9449 §5).
func (h Handler) Token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); media != "application/x-www-form-urlencoded" || r.ParseForm() != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if r.PostForm.Get("grant_type") != "client_credentials" {
		writeTokenError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	if r.PostForm.Get("client_assertion_type") != deviceauth.ClientAssertionType {
		writeTokenError(w, http.StatusBadRequest, "invalid_client")
		return
	}
	token, err := h.service.IssueAccessToken(r.PostForm.Get("client_assertion"), RequestProof(r), r.Header.Get(h.service.ProxyHeader()))
	switch {
	case errors.Is(err, deviceauth.ErrUnsupportedGrant):
		writeTokenError(w, http.StatusBadRequest, "unsupported_grant_type")
	case errors.Is(err, deviceauth.ErrInvalidClient):
		writeTokenError(w, http.StatusBadRequest, "invalid_client")
	case errors.Is(err, deviceauth.ErrProofInvalid):
		writeTokenError(w, http.StatusBadRequest, "invalid_dpop_proof")
	// The user the proxy vouches for is the resource owner credential here.
	case errors.Is(err, deviceauth.ErrUserInvalid):
		writeTokenError(w, http.StatusBadRequest, "invalid_grant")
	case err != nil:
		httpapi.WriteError(w, http.StatusInternalServerError, err)
	default:
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{"access_token": token.Token, "token_type": "DPoP", "expires_in": token.ExpiresIn})
	}
}

func writeTokenError(w http.ResponseWriter, status int, code string) {
	httpapi.WriteJSON(w, status, map[string]string{"error": code})
}

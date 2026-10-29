package devices

import (
	"errors"
	"net/http"

	"github.com/wins/jaz/backend/internal/deviceauth"
	"github.com/wins/jaz/backend/internal/httpapi"
)

// RequestProof reads the DPoP proof of a request from its header or, since
// browsers cannot set headers on a WebSocket handshake, from a subprotocol.
func RequestProof(r *http.Request) deviceauth.Proof {
	jwt := r.Header.Get(deviceauth.ProofHeader)
	if jwt == "" {
		jwt = httpapi.WebSocketProtocol(r, "jaz-dpop.")
	}
	return deviceauth.Proof{JWT: jwt, Method: r.Method, Path: r.URL.Path, Origin: httpapi.RequestBaseURL(r), Local: httpapi.TrustedLocalRequest(r)}
}

// Token is the OAuth token endpoint of proof mode (RFC 6749 §3.2): a device
// authenticates with a private_key_jwt assertion (RFC 7523) and receives a
// DPoP-bound access token (RFC 9449 §5).
func (h Handler) Token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if err := r.ParseForm(); err != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if r.PostForm.Get("grant_type") != "client_credentials" {
		writeTokenError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	if r.PostForm.Get("client_assertion_type") != deviceauth.ClientAssertionType {
		writeTokenError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	token, err := h.service.IssueAccessToken(r.PostForm.Get("client_assertion"), RequestProof(r))
	switch {
	case errors.Is(err, deviceauth.ErrUnsupportedGrant):
		writeTokenError(w, http.StatusBadRequest, "unsupported_grant_type")
	case errors.Is(err, deviceauth.ErrInvalidClient):
		writeTokenError(w, http.StatusUnauthorized, "invalid_client")
	case errors.Is(err, deviceauth.ErrProofInvalid):
		writeTokenError(w, http.StatusBadRequest, "invalid_dpop_proof")
	case err != nil:
		httpapi.WriteError(w, http.StatusInternalServerError, err)
	default:
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{"access_token": token.Token, "token_type": "DPoP", "expires_in": token.ExpiresIn})
	}
}

func writeTokenError(w http.ResponseWriter, status int, code string) {
	httpapi.WriteJSON(w, status, map[string]string{"error": code})
}

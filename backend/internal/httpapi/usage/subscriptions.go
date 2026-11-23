package usage

import (
	"context"
	"net/http"

	"github.com/wins/jaz/backend/internal/acp"
	"github.com/wins/jaz/backend/internal/httpapi"
)

type subscriptionReader interface {
	SubscriptionUsage(context.Context, string) (acp.SubscriptionUsage, error)
}

func NewSubscriptionHandler(reader subscriptionReader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent := r.PathValue("agent")
		switch agent {
		case acp.AgentClaude, acp.AgentCodex, acp.AgentKimi:
		default:
			http.NotFound(w, r)
			return
		}
		usage, err := reader.SubscriptionUsage(r.Context(), agent)
		if err != nil {
			httpapi.WriteError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		httpapi.WriteJSON(w, http.StatusOK, usage)
	})
}

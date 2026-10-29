package app

import (
	"fmt"
	"strings"

	"github.com/wins/jaz/backend/internal/acp"
	"github.com/wins/jaz/backend/internal/canvas"
	"github.com/wins/jaz/backend/internal/deviceauth"
	feedcore "github.com/wins/jaz/backend/internal/feed"
	agentsessionsapi "github.com/wins/jaz/backend/internal/httpapi/agentsessions"
	canvasapi "github.com/wins/jaz/backend/internal/httpapi/canvas"
	previewapi "github.com/wins/jaz/backend/internal/httpapi/preview"
	sessionsapi "github.com/wins/jaz/backend/internal/httpapi/sessions"
	voiceapi "github.com/wins/jaz/backend/internal/httpapi/voice"
	"github.com/wins/jaz/backend/internal/sessionoverview"
	sqlitestore "github.com/wins/jaz/backend/internal/storage/sqlite"
	"github.com/wins/jaz/backend/internal/transcript"
	usagecore "github.com/wins/jaz/backend/internal/usage"
	"github.com/wins/jaz/backend/internal/voice/live"
	"go.uber.org/fx"
)

func HTTPModule() fx.Option {
	return fx.Provide(
		usagecore.NewService,
		feedcore.NewService,
		NewVoiceCredentials,
		NewMobilePush,
		fx.Annotate(live.NewService, fx.From(new(*sqlitestore.Store))),
		fx.Annotate(live.NewTranscript, fx.From(new(*sqlitestore.Store))),
		voiceapi.NewHandler,
		previewapi.NewHandler,
		fx.Annotate(
			transcript.NewService,
			fx.From(new(*sqlitestore.Store), new(*acp.Manager)),
		),
		fx.Annotate(
			sessionoverview.NewService,
			fx.From(new(*sqlitestore.Store), new(*acp.Manager)),
		),
		fx.Annotate(agentsessionsapi.NewHandler, fx.From(new(*acp.Manager))),
		sessionsapi.NewMessagesHandler,
		sessionsapi.NewOverviewHandler,
		fx.Annotate(sessionsapi.NewGoalHandler, fx.From(new(*acp.Manager))),
		canvas.NewService,
		fx.Annotate(canvasapi.NewHandler, fx.From(new(*sqlitestore.Store))),
		NewRoutes,
		NewPublicRoutes,
	)
}

func NewDeviceAuth(store *sqlitestore.Store, cfg Config) (*deviceauth.Service, error) {
	devices := deviceauth.New(store)
	devices.RequireProof = cfg.Devices.RequireProof
	for _, raw := range cfg.Devices.Origins {
		origin := deviceauth.NormalizeOrigin(strings.TrimSpace(raw))
		if origin == "" {
			return nil, fmt.Errorf("jaz.devices.origins: %q is not an http(s) origin", raw)
		}
		devices.Origins = append(devices.Origins, origin)
	}
	if devices.RequireProof && len(devices.Origins) == 0 {
		return nil, fmt.Errorf("jaz.devices.requireproof needs jaz.devices.origins: the origins clients reach this server on")
	}
	return devices, nil
}

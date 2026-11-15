package app

import (
	"context"
	"strings"

	"github.com/charmbracelet/log"
	"github.com/wins/jaz/backend/internal/acp"
	"github.com/wins/jaz/backend/internal/mobilepush"
	"github.com/wins/jaz/backend/internal/serverconfig"
	"github.com/wins/jaz/backend/internal/sessionevents"
	sqlitestore "github.com/wins/jaz/backend/internal/storage/sqlite"
	"go.uber.org/fx"
)

func NewMobilePush(config Config, store *sqlitestore.Store, manager *acp.Manager, serverConfig serverconfig.Config) (*mobilepush.Service, error) {
	publicURL := serverconfig.ClientBaseURL(serverConfig)
	if config.WebPush.PublicURL == "" {
		config.WebPush.PublicURL = publicURL
	}
	if config.WebPush.Subject == "" && strings.HasPrefix(publicURL, "https://") {
		config.WebPush.Subject = publicURL
	}
	sender, err := mobilepush.NewApple(config.Push)
	if err != nil {
		return nil, err
	}
	webPush, err := mobilepush.NewWebPush(config.WebPush)
	if err != nil {
		return nil, err
	}
	return mobilepush.NewService(store, sender, webPush, manager.AwaitsAnswer), nil
}

func StartMobilePush(lc fx.Lifecycle, service *mobilepush.Service, events *sessionevents.Bus, logger *log.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	var done <-chan struct{}
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			done = service.Start(ctx, events, func(err error) {
				logger.Warn("mobile push delivery failed", "error", err)
			})
			return nil
		},
		OnStop: func(stop context.Context) error {
			cancel()
			select {
			case <-done:
				return nil
			case <-stop.Done():
				return stop.Err()
			}
		},
	})
}

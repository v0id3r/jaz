package mobilepush

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

type WebPushConfig struct {
	PublicKey  string
	PrivateKey string
	Subject    string
	PublicURL  string
}

type WebPushSubscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

type WebPushSender interface {
	Enabled() bool
	PublicKey() string
	PublicURL() string
	Send(context.Context, WebPushSubscription, any) error
}

type WebPush struct {
	config WebPushConfig
	client *http.Client
}

func NewWebPush(config WebPushConfig) (*WebPush, error) {
	if config.Subject == "" {
		config.Subject = "https://jaz.chat"
	}
	if config.PublicURL == "" {
		config.PublicURL = "https://jaz.chat"
	}
	if config.PublicKey == "" && config.PrivateKey == "" {
		return &WebPush{config: config, client: &http.Client{Timeout: 10 * time.Second}}, nil
	}
	if config.PublicKey == "" || config.PrivateKey == "" {
		return nil, errors.New("web push requires both VAPID public and private keys")
	}
	return &WebPush{config: config, client: &http.Client{Timeout: 10 * time.Second}}, nil
}

func (w *WebPush) Enabled() bool {
	return w.config.PublicKey != ""
}

func (w *WebPush) PublicKey() string {
	return w.config.PublicKey
}

func (w *WebPush) PublicURL() string {
	return w.config.PublicURL
}

func (w *WebPush) Send(ctx context.Context, subscription WebPushSubscription, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	response, err := webpush.SendNotificationWithContext(ctx, data, &webpush.Subscription{
		Endpoint: subscription.Endpoint,
		Keys:     webpush.Keys{P256dh: subscription.Keys.P256DH, Auth: subscription.Keys.Auth},
	}, &webpush.Options{
		HTTPClient:      w.client,
		Subscriber:      w.config.Subject,
		TTL:             3600,
		VAPIDPublicKey:  w.config.PublicKey,
		VAPIDPrivateKey: w.config.PrivateKey,
	})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
		return ErrInvalidToken
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("web push HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

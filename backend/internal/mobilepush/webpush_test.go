package mobilepush

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/wins/jaz/backend/internal/sessionevents"
	"github.com/wins/jaz/backend/internal/storage"
)

func TestWebPushSendsEncryptedPayloadAndDropsExpiredSubscription(t *testing.T) {
	privateKey, publicKey, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	status := http.StatusCreated
	var body []byte
	var authorization string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		authorization = r.Header.Get("Authorization")
		w.WriteHeader(status)
	}))
	defer server.Close()
	sender, err := NewWebPush(WebPushConfig{PublicKey: publicKey, PrivateKey: privateKey})
	if err != nil {
		t.Fatal(err)
	}
	sender.client = server.Client()
	subscription := testWebPushSubscription(t, server.URL)
	if err := sender.Send(t.Context(), subscription, map[string]string{"title": "Done"}); err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 || authorization == "" {
		t.Fatalf("push was not encrypted and authorized: body=%d auth=%q", len(body), authorization)
	}
	status = http.StatusGone
	if err := sender.Send(t.Context(), subscription, map[string]string{"title": "Done"}); err != ErrInvalidToken {
		t.Fatalf("expired subscription error = %v", err)
	}
}

func TestWebPushDeliveryUsesPairedDeviceAndDedupes(t *testing.T) {
	service, store, _, session := pushFixture(t, "")
	web := &capturingWebSender{}
	service.webPush = web
	if err := service.RegisterWebPush("phone", testWebPushSubscription(t, "https://push.example")); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSessionStatus(session.ID, "running", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSessionStatus(session.ID, "idle", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteSession(session.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	event := sessionevents.Event{SessionID: session.ID, Type: sessionevents.TypeTurnFinished, ACP: &sessionevents.ACPEvent{State: "idle", StopReason: "end_turn", Assistant: "Ready"}}
	for range 2 {
		if err := service.process(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	if len(web.payloads) != 1 || web.payloads[0]["session_id"] != session.ID {
		t.Fatalf("web payloads = %#v", web.payloads)
	}
	if web.payloads[0]["web_push"] != float64(8030) || web.payloads[0]["notification"].(map[string]any)["navigate"] != "https://jaz.example/sessions/"+session.ID+"?from=push" {
		t.Fatalf("declarative navigation = %#v", web.payloads[0])
	}
	var saved deviceRegistration
	setting, err := store.LoadSetting(namespace, "phone")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(setting.Value, &saved); err != nil || saved.WebPush == nil {
		t.Fatalf("subscription was not persisted: %v", err)
	}
}

func TestActiveSessionSuppressesWebPushAcrossDevices(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "cleared", true: "expired"}[expired], func(t *testing.T) {
			service, store, _, session := pushFixture(t, "")
			web := &capturingWebSender{}
			service.webPush = web
			if _, err := store.SaveApprovedDevice(storage.SaveApprovedDevice{
				ID: "desktop", Name: "Desktop", Kind: storage.DeviceKindDesktop, TokenHash: "desktop-hash", CreatedAt: time.Now(), ApprovedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			for _, device := range []string{"phone", "desktop"} {
				if err := service.RegisterWebPush(device, testWebPushSubscription(t, "https://push.example/"+device)); err != nil {
					t.Fatal(err)
				}
			}
			if err := service.MarkPresence("desktop", session.ID); err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateSessionStatus(session.ID, storage.StatusRunning, "", time.Time{}); err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateSessionStatus(session.ID, storage.StatusIdle, "", time.Time{}); err != nil {
				t.Fatal(err)
			}
			if err := store.CompleteSession(session.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
			event := sessionevents.Event{SessionID: session.ID, Type: sessionevents.TypeTurnFinished, ACP: &sessionevents.ACPEvent{State: "idle", StopReason: "end_turn", Assistant: "Ready"}}
			if err := service.process(t.Context(), event); err != nil {
				t.Fatal(err)
			}
			if len(web.payloads) != 0 {
				t.Fatalf("active session sent %d pushes", len(web.payloads))
			}
			if expired {
				if err := service.update("desktop", func(device *deviceRegistration) {
					device.Presence.ExpiresAt = time.Now().Add(-time.Second)
				}); err != nil {
					t.Fatal(err)
				}
			} else if err := service.MarkPresence("desktop", ""); err != nil {
				t.Fatal(err)
			}
			if err := service.process(t.Context(), event); err != nil {
				t.Fatal(err)
			}
			if len(web.payloads) != 2 {
				t.Fatalf("restored presence delivery sent %d pushes", len(web.payloads))
			}
		})
	}
}

func TestWebPushRejectsInvalidRegistration(t *testing.T) {
	service, _, _, _ := pushFixture(t, "")
	for _, subscription := range []WebPushSubscription{{Endpoint: "http://push.example"}, testWebPushSubscription(t, "https://push.example")} {
		if len(subscription.Keys.Auth) > 0 {
			subscription.Keys.Auth = "bad"
		}
		if err := service.RegisterWebPush("phone", subscription); err == nil {
			t.Fatal("invalid subscription accepted")
		}
	}
}

func TestWebPushConfigNeedsAKeyPair(t *testing.T) {
	if _, err := NewWebPush(WebPushConfig{PublicKey: "only-public"}); err == nil {
		t.Fatal("partial VAPID config accepted")
	}
}

func testWebPushSubscription(t *testing.T, endpoint string) WebPushSubscription {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var subscription WebPushSubscription
	subscription.Endpoint = endpoint
	subscription.Keys.P256DH = base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())
	subscription.Keys.Auth = base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef"))
	return subscription
}

type capturingWebSender struct {
	payloads []map[string]any
}

func (s *capturingWebSender) Enabled() bool     { return true }
func (s *capturingWebSender) PublicKey() string { return "public" }
func (s *capturingWebSender) PublicURL() string { return "https://jaz.example" }
func (s *capturingWebSender) Send(_ context.Context, _ WebPushSubscription, payload any) error {
	data, _ := json.Marshal(payload)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	s.payloads = append(s.payloads, decoded)
	return nil
}

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/push"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

// recordingPush é o push.Sender dos testes: aceita tudo e guarda as mensagens.
type recordingPush struct {
	mu  sync.Mutex
	got []push.Message
}

func (r *recordingPush) Send(_ context.Context, _ dbgen.PushSubscription, body []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var m push.Message
	_ = json.Unmarshal(body, &m)
	r.got = append(r.got, m)
	return http.StatusCreated, nil
}

func (r *recordingPush) messages() []push.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]push.Message(nil), r.got...)
}

func TestPushSubscriptionTestAndPreferences(t *testing.T) {
	e := setup(t)
	status := call[struct {
		Enabled       bool   `json:"enabled"`
		PublicKey     string `json:"public_key"`
		Subscriptions int    `json:"subscriptions"`
	}](t, e, http.MethodGet, "/api/v1/push/key", "", http.StatusOK)
	require.True(t, status.Enabled)
	require.Equal(t, "BPUB", status.PublicKey)
	require.Zero(t, status.Subscriptions)

	call[struct{}](t, e, http.MethodPost, "/api/v1/push/subscriptions", `{"endpoint":"http://inseguro","keys":{"p256dh":"k","auth":"a"}}`, http.StatusUnprocessableEntity)
	call[struct{}](t, e, http.MethodPost, "/api/v1/push/subscriptions", `{"endpoint":"https://push.example/1","keys":{}}`, http.StatusUnprocessableEntity)
	res := e.do(t, http.MethodPost, "/api/v1/push/subscriptions", `{"endpoint":"https://push.example/1","keys":{"p256dh":"k","auth":"a"}}`, true)
	_ = res.Body.Close()
	require.Equal(t, http.StatusCreated, res.StatusCode)

	sent := call[map[string]int](t, e, http.MethodPost, "/api/v1/push/test", "", http.StatusOK)
	require.Equal(t, 1, sent["sent"])
	require.Equal(t, "Notificações funcionando. Bons troféus!", e.push.messages()[0].Body)

	prefs := call[push.Preferences](t, e, http.MethodGet, "/api/v1/notifications/preferences", "", http.StatusOK)
	require.Equal(t, push.DefaultPreferences(), prefs)
	saved := call[push.Preferences](t, e, http.MethodPut, "/api/v1/notifications/preferences", `{"unlocks":true,"completions":true,"reconnect":true,"errors":false,"ai_ready":false}`, http.StatusOK)
	require.False(t, saved.AIReady)
	call[struct{}](t, e, http.MethodPut, "/api/v1/notifications/preferences", `{"desconhecido":true}`, http.StatusBadRequest)

	del := e.do(t, http.MethodDelete, "/api/v1/push/subscriptions", `{"endpoint":"https://push.example/1"}`, true)
	_ = del.Body.Close()
	require.Equal(t, http.StatusNoContent, del.StatusCode)
}

func TestUnlockPushAfterSync(t *testing.T) {
	var unlocked atomic.Bool
	e := setupWith(t, func(string) syncpkg.SteamAPI { return unlockingSteam{unlocked: &unlocked} })
	res := e.do(t, http.MethodPost, "/api/v1/push/subscriptions", `{"endpoint":"https://push.example/1","keys":{"p256dh":"k","auth":"a"}}`, true)
	_ = res.Body.Close()
	integ := call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/integrations/steam", `{"api_key":"K","steam_id_or_vanity":"76561197960000000"}`, http.StatusCreated)
	e.drain(t)
	require.Empty(t, e.push.messages(), "sync inicial não notifica")

	unlocked.Store(true)
	call[struct{}](t, e, http.MethodPost, "/api/v1/integrations/"+integ.ID+"/sync", `{"scope":"incremental"}`, http.StatusAccepted)
	e.drain(t)
	msgs := e.push.messages()
	require.Len(t, msgs, 1)
	require.Equal(t, push.KindCompletions, msgs[0].Kind)
	require.Equal(t, "100%! Portal 2", msgs[0].Title)

	// Já notificado: outro sync não repete.
	call[struct{}](t, e, http.MethodPost, "/api/v1/integrations/"+integ.ID+"/sync", `{"scope":"incremental"}`, http.StatusAccepted)
	e.drain(t)
	require.Len(t, e.push.messages(), 1)
}

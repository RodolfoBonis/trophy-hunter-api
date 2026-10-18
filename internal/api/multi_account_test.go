package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/steam"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

const (
	steamMain = "76561197960000000"
	steamAlt  = "76561197960000001"
)

// accountFake simula a Steam com dados POR CONTA (SteamID): biblioteca e conquistas diferentes.
// Registra as API keys usadas, para provar o reaproveitamento da key na 2ª conta.
type accountFake struct {
	mu   *sync.Mutex
	keys *[]string
	key  string
}

var accountLibs = map[string][]steam.OwnedGame{
	steamMain: {
		{AppID: 620, Name: "Portal 2", RtimeLastPlayed: 1700000000, HasCommunityVisibleStats: true},
		{AppID: 400, Name: "Portal", RtimeLastPlayed: 1600000000, HasCommunityVisibleStats: true},
	},
	steamAlt: {
		{AppID: 620, Name: "Portal 2", RtimeLastPlayed: 1710000000, HasCommunityVisibleStats: true},
	},
}

// Principal: Portal 2 com 1/2; Alt: Portal 2 com 2/2 (mais avançada). Portal só na principal (0/2).
var accountAch = map[string]map[int][]steam.PlayerAchievement{
	steamMain: {
		620: {{APIName: "A", Achieved: true, UnlockTime: 1700000500}, {APIName: "B"}},
		400: {{APIName: "A"}, {APIName: "B"}},
	},
	steamAlt: {
		620: {{APIName: "A", Achieved: true, UnlockTime: 1710000100}, {APIName: "B", Achieved: true, UnlockTime: 1710000200}},
	},
}

func (f accountFake) GetOwnedGames(_ context.Context, id string) ([]steam.OwnedGame, error) {
	return accountLibs[id], nil
}
func (f accountFake) GetRecentlyPlayedGames(context.Context, string) ([]steam.OwnedGame, error) {
	return nil, nil
}
func (f accountFake) GetPlayerAchievements(_ context.Context, id string, appID int) ([]steam.PlayerAchievement, error) {
	return accountAch[id][appID], nil
}
func (f accountFake) GetSchemaForGame(context.Context, int) (string, []steam.SchemaAchievement, error) {
	return "g", []steam.SchemaAchievement{{Name: "A", DisplayName: "A"}, {Name: "B", DisplayName: "B"}}, nil
}
func (f accountFake) GetGlobalAchievementPercentages(context.Context, int) (map[string]float64, error) {
	return map[string]float64{"A": 50, "B": 10}, nil
}
func (f accountFake) GetPlayerSummary(_ context.Context, id string) (steam.PlayerSummary, error) {
	f.mu.Lock()
	*f.keys = append(*f.keys, f.key)
	f.mu.Unlock()
	return steam.PlayerSummary{SteamID: id, PersonaName: "persona-" + id[len(id)-1:], CommunityVisibleState: 3}, nil
}
func (f accountFake) GetAppAssets(context.Context, []int) (map[int]steam.AppAssets, error) {
	return nil, nil
}
func (f accountFake) ResolveVanityURL(context.Context, string) (string, error) { return steamAlt, nil }

type gameAccount struct {
	IntegrationID string `json:"integration_id"`
	Label         string `json:"label"`
	GameID        string `json:"game_id"`
	Progress      struct {
		Earned int `json:"earned"`
		Total  int `json:"total"`
	} `json:"progress"`
}

type listItem struct {
	ID         string `json:"id"`
	ExternalID string `json:"external_id"`
	Progress   struct {
		Earned int `json:"earned"`
	} `json:"progress"`
	Accounts []gameAccount `json:"accounts"`
}

// call faz a requisição autenticada, confere o status e decodifica o JSON (fecha o body).
func call[T any](t *testing.T, e *testEnv, method, path, reqBody string, want int) T {
	t.Helper()
	resp := e.do(t, method, path, reqBody, true)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, want, resp.StatusCode, string(body))
	var v T
	require.NoError(t, json.Unmarshal(body, &v), string(body))
	return v
}

func TestMultipleSteamAccounts(t *testing.T) {
	var mu sync.Mutex
	var usedKeys []string
	e := setupWith(t, func(key string) syncpkg.SteamAPI { return accountFake{mu: &mu, keys: &usedKeys, key: key} })

	// 1ª conta sem API key: obrigatória (não há key para reaproveitar).
	resp := e.do(t, http.MethodPost, "/api/v1/integrations/steam", `{"label":"Principal","steam_id_or_vanity":"`+steamMain+`"}`, true)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)

	main := call[struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}](t, e, http.MethodPost, "/api/v1/integrations/steam",
		`{"label":"Principal","api_key":"KEY-MAIN","steam_id_or_vanity":"`+steamMain+`"}`, http.StatusCreated)
	require.Equal(t, "Principal", main.Label)

	// 2ª conta SEM api_key (via vanity): reaproveita a key da principal.
	mu.Lock()
	usedKeys = nil
	mu.Unlock()
	alt := call[struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}](t, e, http.MethodPost, "/api/v1/integrations/steam",
		`{"label":"Alt","steam_id_or_vanity":"minha-alt"}`, http.StatusCreated)
	require.Equal(t, []string{"KEY-MAIN"}, usedKeys, "a 2ª conta usa a key da 1ª")

	// Mesma conta de novo → 409.
	dup := e.do(t, http.MethodPost, "/api/v1/integrations/steam", `{"steam_id_or_vanity":"`+steamAlt+`"}`, true)
	_ = dup.Body.Close()
	require.Equal(t, http.StatusConflict, dup.StatusCode)

	e.drain(t)

	integs := call[[]struct {
		ID     string `json:"id"`
		Label  string `json:"label"`
		Status string `json:"status"`
	}](t, e, http.MethodGet, "/api/v1/integrations", "", http.StatusOK)
	require.Len(t, integs, 2)
	for _, i := range integs {
		require.Equal(t, "connected", i.Status, i.Label)
	}

	// Biblioteca: UMA entrada por jogo; Portal 2 representado pela Alt (2/2), com as duas contas.
	page := call[struct {
		Items []listItem `json:"items"`
		Total int        `json:"total"`
	}](t, e, http.MethodGet, "/api/v1/games?sort=title", "", http.StatusOK)
	require.Equal(t, 2, page.Total)
	byExt := map[string]listItem{}
	for _, it := range page.Items {
		byExt[it.ExternalID] = it
	}
	p2 := byExt["620"]
	require.Equal(t, 2, p2.Progress.Earned, "best = conta mais avançada")
	require.Len(t, p2.Accounts, 2)
	require.Equal(t, alt.ID, p2.Accounts[0].IntegrationID, "mais avançada primeiro")
	require.Equal(t, p2.ID, p2.Accounts[0].GameID)
	require.Equal(t, main.ID, p2.Accounts[1].IntegrationID)
	require.Equal(t, 1, p2.Accounts[1].Progress.Earned)
	require.Len(t, byExt["400"].Accounts, 1)

	// Filtro por conta: só a principal → Portal 2 com o progresso DELA.
	only := call[struct {
		Items []listItem `json:"items"`
	}](t, e, http.MethodGet, "/api/v1/games?sort=title&integration_id="+main.ID, "", http.StatusOK)
	require.Len(t, only.Items, 2)
	for _, it := range only.Items {
		require.Len(t, it.Accounts, 1)
		require.Equal(t, main.ID, it.Accounts[0].IntegrationID)
		if it.ExternalID == "620" {
			require.Equal(t, 1, it.Progress.Earned)
		}
	}

	// Detalhe da linha da principal traz a conta dela e as irmãs.
	detail := call[struct {
		IntegrationID string        `json:"integration_id"`
		AccountLabel  string        `json:"account_label"`
		Accounts      []gameAccount `json:"accounts"`
	}](t, e, http.MethodGet, "/api/v1/games/"+p2.Accounts[1].GameID, "", http.StatusOK)
	require.Equal(t, main.ID, detail.IntegrationID)
	require.Equal(t, "Principal", detail.AccountLabel)
	require.Len(t, detail.Accounts, 2)

	// Renomear.
	renamed := call[struct {
		Label string `json:"label"`
	}](t, e, http.MethodPatch, "/api/v1/integrations/"+alt.ID, `{"label":"Smurf"}`, http.StatusOK)
	require.Equal(t, "Smurf", renamed.Label)

	// Remover a Alt: cascateia só os jogos dela; Portal 2 volta a ser representado pela principal.
	del := e.do(t, http.MethodDelete, "/api/v1/integrations/"+alt.ID, "", true)
	_ = del.Body.Close()
	require.Equal(t, http.StatusNoContent, del.StatusCode)
	after := call[struct {
		Items []listItem `json:"items"`
	}](t, e, http.MethodGet, "/api/v1/games?sort=title", "", http.StatusOK)
	require.Len(t, after.Items, 2)
	for _, it := range after.Items {
		require.Len(t, it.Accounts, 1)
		require.Equal(t, main.ID, it.Accounts[0].IntegrationID)
	}
}

func urlEscape(s string) string { return url.QueryEscape(s) }

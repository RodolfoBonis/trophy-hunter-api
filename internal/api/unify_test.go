package api_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/psn"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

// psnPortal: a PSN tem "Portal 2™" (normaliza igual ao "Portal 2" da Steam) e "Portal Knights".
type psnPortal struct{}

func (psnPortal) ExchangeNPSSO(context.Context, string) (psn.Tokens, error) {
	return psn.Tokens{AccessToken: "A", AccessExpiresAt: time.Now().Add(time.Hour), RefreshToken: "R", RefreshExpiresAt: time.Now().Add(240 * time.Hour)}, nil
}
func (psnPortal) Refresh(context.Context, string) (psn.Tokens, error) {
	return psn.Tokens{}, psn.ErrInvalidGrant
}
func (psnPortal) TrophySummary(context.Context, string) (psn.Summary, error) {
	return psn.Summary{AccountID: "77"}, nil
}
func (psnPortal) Profile(context.Context, string, string) (psn.Profile, error) {
	return psn.Profile{OnlineID: "psn_user"}, nil
}
func (psnPortal) TrophyTitles(context.Context, string) ([]psn.TrophyTitle, error) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return []psn.TrophyTitle{
		{NpCommunicationID: "NPWR_P2", NpServiceName: "trophy", TrophyTitleName: "Portal 2™", LastUpdatedDateTime: at},
		{NpCommunicationID: "NPWR_PK", NpServiceName: "trophy", TrophyTitleName: "Portal Knights", LastUpdatedDateTime: at},
	}, nil
}
func (psnPortal) TitleTrophies(context.Context, string, string, string) ([]psn.TrophyDef, error) {
	return []psn.TrophyDef{{TrophyID: 1, TrophyType: "bronze", TrophyName: "X", TrophyGroupID: "default"}}, nil
}
func (psnPortal) TitleGroups(context.Context, string, string, string) ([]psn.TrophyGroup, error) {
	return nil, nil
}
func (psnPortal) UserTrophies(context.Context, string, string, string) ([]psn.UserTrophy, error) {
	return []psn.UserTrophy{{TrophyID: 1, Earned: true, EarnedDateTime: time.Now(), TrophyEarnedRate: "50"}}, nil
}

func TestCrossPlatformUnify(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	e := setupFull(t, func(key string) syncpkg.SteamAPI { return accountFake{mu: &mu, keys: &keys, key: key} }, psnPortal{})
	call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/integrations/steam", `{"api_key":"K","steam_id_or_vanity":"`+steamMain+`"}`, http.StatusCreated)
	call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/integrations/psn", `{"npsso":"N"}`, http.StatusCreated)
	e.drain(t)
	e.sync.AutoLink(context.Background())

	type item struct {
		ID            string `json:"id"`
		Platform      string `json:"platform"`
		ExternalID    string `json:"external_id"`
		CrossPlatform bool   `json:"cross_platform"`
	}
	type page struct {
		Items []item
		Total int
	}
	list := func(query string) map[string]item {
		p := call[page](t, e, http.MethodGet, "/api/v1/games?sort=title&has_achievements=all"+query, "", http.StatusOK)
		require.Len(t, p.Items, p.Total)
		out := map[string]item{}
		for _, it := range p.Items {
			out[it.ExternalID] = it
		}
		return out
	}
	// Por plataforma, cada versão aparece; sem filtro, o jogo unificado vira UM card (o mais avançado).
	byExt := list("&platform=steam")
	for k, v := range list("&platform=psn") {
		byExt[k] = v
	}
	require.True(t, byExt["620"].CrossPlatform, "Portal 2 (Steam) ↔ Portal 2™ (PSN) — mesmo título normalizado")
	require.True(t, byExt["NPWR_P2"].CrossPlatform)
	require.False(t, byExt["NPWR_PK"].CrossPlatform, "Portal Knights não é Portal")
	require.False(t, byExt["400"].CrossPlatform)
	all := list("")
	_, steamP2 := all["620"]
	_, psnP2 := all["NPWR_P2"]
	require.True(t, steamP2 != psnP2, "Portal 2 aparece uma vez só na biblioteca")
	require.Len(t, all, len(byExt)-1)

	type detail struct {
		CrossPlatform []struct {
			GameID   string `json:"game_id"`
			Platform string `json:"platform"`
			Earned   int    `json:"earned"`
		} `json:"cross_platform"`
		UnifyLocked bool `json:"unify_locked"`
	}
	d := call[detail](t, e, http.MethodGet, "/api/v1/games/"+byExt["620"].ID, "", http.StatusOK)
	require.Len(t, d.CrossPlatform, 1)
	require.Equal(t, "psn", d.CrossPlatform[0].Platform)
	require.Equal(t, 1, d.CrossPlatform[0].Earned)

	// Desvincular trava a decisão: a unificação automática não religa.
	unl := e.do(t, http.MethodDelete, "/api/v1/games/"+byExt["620"].ID+"/link", "", true)
	_ = unl.Body.Close()
	require.Equal(t, http.StatusNoContent, unl.StatusCode)
	e.sync.AutoLink(context.Background())
	d = call[detail](t, e, http.MethodGet, "/api/v1/games/"+byExt["620"].ID, "", http.StatusOK)
	require.Empty(t, d.CrossPlatform)
	require.True(t, d.UnifyLocked)
	require.Len(t, list(""), len(byExt), "desvinculado, volta a ter um card por plataforma")

	// Candidatos da outra plataforma e vínculo manual (Portal Steam ↔ Portal Knights PSN).
	cands := call[[]struct {
		GameID string `json:"game_id"`
		Title  string `json:"title"`
	}](t, e, http.MethodGet, "/api/v1/games/"+byExt["400"].ID+"/link-candidates?q=portal", "", http.StatusOK)
	require.NotEmpty(t, cands)
	for _, c := range cands {
		require.NotEqual(t, "Portal", c.Title, "só jogos da OUTRA plataforma")
	}
	link := e.do(t, http.MethodPost, "/api/v1/games/"+byExt["400"].ID+"/link", `{"other_game_id":"`+byExt["NPWR_PK"].ID+`"}`, true)
	_ = link.Body.Close()
	require.Equal(t, http.StatusNoContent, link.StatusCode)
	d = call[detail](t, e, http.MethodGet, "/api/v1/games/"+byExt["400"].ID, "", http.StatusOK)
	require.Len(t, d.CrossPlatform, 1)
	require.Equal(t, byExt["NPWR_PK"].ID, d.CrossPlatform[0].GameID)

	same := e.do(t, http.MethodPost, "/api/v1/games/"+byExt["400"].ID+"/link", `{"other_game_id":"`+byExt["620"].ID+`"}`, true)
	_ = same.Body.Close()
	require.Equal(t, http.StatusUnprocessableEntity, same.StatusCode, "mesma plataforma não vincula")
}

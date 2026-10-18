package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

type searchResult struct {
	Games []struct {
		ID, Title, Platform string
	}
	Trophies []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		GameID string `json:"game_id"`
		Earned bool   `json:"earned"`
	}
}

func TestSearchGamesAndTrophies(t *testing.T) {
	e := setup(t)
	call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/integrations/steam", `{"api_key":"K","steam_id_or_vanity":"76561197960000000"}`, http.StatusCreated)
	e.drain(t)

	short := call[searchResult](t, e, http.MethodGet, "/api/v1/search?q=p", "", http.StatusOK)
	require.Empty(t, short.Games, "1 caractere não busca")

	res := call[searchResult](t, e, http.MethodGet, "/api/v1/search?q=portal", "", http.StatusOK)
	require.Len(t, res.Games, 1)
	require.Equal(t, "Portal 2", res.Games[0].Title)

	tr := call[searchResult](t, e, http.MethodGet, "/api/v1/search?q=venc", "", http.StatusOK)
	require.Len(t, tr.Trophies, 1)
	require.Equal(t, "Venceu", tr.Trophies[0].Name)
	require.Equal(t, res.Games[0].ID, tr.Trophies[0].GameID)

	hidden := call[searchResult](t, e, http.MethodGet, "/api/v1/search?q=segredo", "", http.StatusOK)
	require.Empty(t, hidden.Trophies, "oculto ainda não obtido não aparece (spoiler)")

	special := call[searchResult](t, e, http.MethodGet, "/api/v1/search?q=%25%25", "", http.StatusOK)
	require.Empty(t, special.Games, "% é literal, não curinga")
}

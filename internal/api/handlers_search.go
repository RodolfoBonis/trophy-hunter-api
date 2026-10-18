package api

import (
	"net/http"
	"strings"
	"unicode/utf8"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

type searchGameDTO struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	Platform      string  `json:"platform"`
	ExternalID    string  `json:"external_id"`
	Cover         string  `json:"cover"`
	Hero          string  `json:"hero"`
	CompletionPct float64 `json:"completion_pct"`
}

type searchTrophyDTO struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Icon      string   `json:"icon"`
	Earned    bool     `json:"earned"`
	RarityPct *float64 `json:"rarity_pct"`
	GameID    string   `json:"game_id"`
	GameTitle string   `json:"game_title"`
	Platform  string   `json:"platform"`
}

type searchResultDTO struct {
	Games    []searchGameDTO   `json:"games"`
	Trophies []searchTrophyDTO `json:"trophies"`
}

// search — GET /search?q=: jogos e troféus para a paleta de comandos (mín. 2 caracteres).
func (h *Handlers) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	out := searchResultDTO{Games: []searchGameDTO{}, Trophies: []searchTrophyDTO{}}
	if utf8.RuneCountInString(q) < 2 {
		writeJSON(w, http.StatusOK, out)
		return
	}
	if utf8.RuneCountInString(q) > 100 {
		httperr.Write(w, r, httperr.Validation("busca longa demais", httperr.FieldError{Field: "q", Code: "too_long"}))
		return
	}
	ctx := r.Context()
	// Título normalizado (sem ™/pontuação); vazio (ex.: só símbolos) casaria com tudo → não busca.
	var games []dbgen.SearchGamesRow
	if norm := syncpkg.NormalizeTitle(q); norm != "" {
		var err error
		if games, err = h.q.SearchGames(ctx, syncpkg.EscapeLike(norm)); err != nil {
			writeInternal(w, r, err)
			return
		}
	}
	for _, g := range games {
		out.Games = append(out.Games, searchGameDTO{
			ID: uuidStr(g.ID), Title: g.Title, Platform: g.Platform, ExternalID: g.ExternalID,
			Cover: g.Cover, Hero: g.Hero, CompletionPct: g.CompletionPct,
		})
	}
	trophies, err := h.q.SearchTrophies(ctx, syncpkg.EscapeLike(q))
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	for _, t := range trophies {
		out.Trophies = append(out.Trophies, searchTrophyDTO{
			ID: uuidStr(t.ID), Name: t.Name, Type: t.Type, Icon: t.Icon, Earned: t.Earned, RarityPct: t.RarityPct,
			GameID: uuidStr(t.GameID), GameTitle: t.GameTitle, Platform: t.Platform,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/progress"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

var allowedSorts = map[string]bool{
	"completion": true, "remaining": true, "last_played": true, "title": true, "rarity": true,
}

var allowedStatuses = map[string]bool{
	"completed": true, "platinum": true, "almost": true,
	"in_progress": true, "not_started": true, "abandoned": true,
}

// listGames lista a biblioteca com filtros/ordenação/paginação — GET /games.
func (h *Handlers) listGames(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()

	var platform *string
	if p := qp.Get("platform"); p != "" {
		if !validPlatform(p) {
			httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "plataforma inválida"))
			return
		}
		platform = &p
	}

	var status *string
	if s := qp.Get("status"); s != "" {
		if !allowedStatuses[s] {
			httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "status inválido"))
			return
		}
		status = &s
	}

	// Busca: normaliza q com a MESMA normalização de normalized_title e escapa curingas de ILIKE
	// (finding 19). Se nada sobrar após normalizar, não filtra.
	var search *string
	if raw := strings.TrimSpace(qp.Get("q")); raw != "" {
		if norm := syncpkg.EscapeLike(syncpkg.NormalizeTitle(raw)); norm != "" {
			search = &norm
		}
	}

	// has_achievements: "true" (default) | "false" | "all".
	hasAch := func() *bool {
		switch qp.Get("has_achievements") {
		case "", "true":
			v := true
			return &v
		case "false":
			v := false
			return &v
		default: // "all" ou qualquer outro valor → não filtra
			return nil
		}
	}()

	// integration_id: restringe a biblioteca a UMA conta (default: todas, agrupadas por jogo).
	var integrationID pgtype.UUID
	if v := qp.Get("integration_id"); v != "" {
		id, err := parseUUIDParam(v)
		if err != nil {
			httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "integration_id inválido"))
			return
		}
		integrationID = id
	}

	sort := qp.Get("sort")
	if sort == "" {
		sort = "last_played"
	}
	if !allowedSorts[sort] {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "sort inválido"))
		return
	}

	limit := clamp(atoiDefault(qp.Get("limit"), 50), 1, 200)
	offset := atoiDefault(qp.Get("offset"), 0)
	if offset < 0 {
		offset = 0
	}

	ctx := r.Context()
	rows, err := h.q.ListGames(ctx, dbgen.ListGamesParams{
		Platform: platform, HasAchievements: hasAch, Q: search, Status: status, IntegrationID: integrationID,
		Sort: sort, Lim: int32(limit), Off: int32(offset), Almost: h.almostThreshold, //nolint:gosec // valores clampados
	})
	if err != nil {
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}
	total, err := h.q.CountGames(ctx, dbgen.CountGamesParams{
		Platform: platform, HasAchievements: hasAch, Q: search, Status: status, IntegrationID: integrationID,
		Almost: h.almostThreshold,
	})
	if err != nil {
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}

	// Contas de cada jogo da página (uma query para a página toda). Com filtro de conta, o card só
	// mostra aquela conta; sem filtro, todas as contas que possuem o jogo.
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, gameKey(row.Platform, row.ExternalID))
	}
	accounts, err := h.gameAccounts(ctx, keys)
	if err != nil {
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}

	items := make([]gameListItemDTO, 0, len(rows))
	for _, row := range rows {
		item := toListItemDTO(row)
		item.Accounts = accountsFor(accounts[gameKey(row.Platform, row.ExternalID)], row.HasPlatinum, integrationID)
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, gameListPageDTO{Items: items, Total: int(total), Limit: limit, Offset: offset})
}

// getGame devolve o detalhe do jogo com grupos, troféus e progresso — GET /games/{id}.
func (h *Handlers) getGame(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	gameUUID, err := parseUUIDParam(id)
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "id inválido"))
		return
	}
	ctx := r.Context()
	game, err := h.q.GetGameByID(ctx, gameUUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", "jogo não encontrado"))
			return
		}
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}

	groups, err := h.q.ListTrophyGroupsByGame(ctx, gameUUID)
	if err != nil {
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}
	trophies, err := h.q.ListTrophiesByGame(ctx, gameUUID)
	if err != nil {
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}

	baseByGroup := make(map[string]bool, len(groups))
	for _, g := range groups {
		baseByGroup[uuidStr(g.ID)] = g.IsBase
	}

	// Progresso (pacote puro) a partir dos troféus com is_base do grupo.
	pt := make([]progress.Trophy, 0, len(trophies))
	trophiesByGroup := make(map[string][]trophyDTO, len(groups))
	for _, t := range trophies {
		gid := uuidStr(t.GroupID)
		isBase := baseByGroup[gid]
		pt = append(pt, progress.Trophy{Type: t.Type, Earned: t.Earned, IsBase: isBase})
		trophiesByGroup[gid] = append(trophiesByGroup[gid], toTrophyDTO(t))
	}
	summary := progress.Compute(pt, game.Abandoned, progress.Config{AlmostThreshold: h.almostThreshold})

	groupDTOs := make([]trophyGroupDTO, 0, len(groups))
	for _, g := range groups {
		gid := uuidStr(g.ID)
		ts := trophiesByGroup[gid]
		if ts == nil {
			ts = []trophyDTO{}
		}
		groupDTOs = append(groupDTOs, trophyGroupDTO{
			ID: gid, ExternalID: g.ExternalID, Name: g.Name, IsBase: g.IsBase, Icon: g.Icon, Trophies: ts,
		})
	}

	integ, err := h.q.GetIntegration(ctx, game.IntegrationID)
	if err != nil {
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}
	accounts, err := h.gameAccounts(ctx, []string{gameKey(game.Platform, game.ExternalID)})
	if err != nil {
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}

	writeJSON(w, http.StatusOK, gameDetailDTO{
		gameDTO:       toGameDTO(game),
		IntegrationID: uuidStr(integ.ID),
		AccountLabel:  integ.Label,
		Progress:      summary,
		Groups:        groupDTOs,
		Accounts:      accountsFor(accounts[gameKey(game.Platform, game.ExternalID)], game.HasPlatinum, pgtype.UUID{}),
	})
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// gameKey identifica o MESMO jogo entre contas (platform + id externo).
func gameKey(platform, externalID string) string { return platform + ":" + externalID }

// gameAccounts carrega, numa query, as contas que possuem cada jogo (chave → contas, mais avançada
// primeiro).
func (h *Handlers) gameAccounts(ctx context.Context, keys []string) (map[string][]dbgen.ListGameAccountsRow, error) {
	out := make(map[string][]dbgen.ListGameAccountsRow, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := h.q.ListGameAccounts(ctx, dbgen.ListGameAccountsParams{Keys: keys, Almost: h.almostThreshold})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		k := gameKey(r.Platform, r.ExternalID)
		out[k] = append(out[k], r)
	}
	return out, nil
}

// accountsFor converte as contas de um jogo em DTO; com only válido, mantém só aquela conta.
func accountsFor(rows []dbgen.ListGameAccountsRow, hasPlatinum bool, only pgtype.UUID) []gameAccountDTO {
	out := make([]gameAccountDTO, 0, len(rows))
	for _, r := range rows {
		if only.Valid && r.IntegrationID != only {
			continue
		}
		out = append(out, toGameAccountDTO(r, hasPlatinum))
	}
	return out
}

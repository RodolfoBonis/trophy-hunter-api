package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
)

// Social entre membros: perfil do amigo, "amigos neste jogo", feed e ranking.
//
// Regras (decisão do produto):
//   - OPT-IN: ninguém aparece para os outros até ligar "compartilhar meu progresso" (PUT /me/sharing);
//   - RECÍPROCO: só vê os amigos quem também compartilha (senão 403 sharing_disabled);
//   - quem não compartilha (ou não está ativo) é invisível: id de outra pessoa → 404, nunca 403.
//   - só leitura: nada de notas, credenciais ou contas da outra pessoa sai daqui.

const (
	feedDefault = 40
	feedMax     = 100
)

// errSharingDisabled: quem pede ainda não compartilha — a UI mostra o convite para ligar.
func writeSharingDisabled(w http.ResponseWriter, r *http.Request) {
	httperr.Write(w, r, httperr.New(httperr.KindForbidden, "Compartilhamento desligado",
		"ligue \"compartilhar meu progresso\" para ver o progresso dos amigos").WithCode("sharing_disabled"))
}

// requireSharing libera só quem compartilha (reciprocidade).
func requireSharing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !currentUser(r).ShareProgress {
			writeSharingDisabled(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type socialUserDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	IsMe bool   `json:"is_me"`
}

func (h *Handlers) socialUser(r *http.Request, id pgtype.UUID, name string) socialUserDTO {
	if name == "" {
		name = "Jogador"
	}
	return socialUserDTO{ID: uuidStr(id), Name: name, IsMe: id == currentUser(r).ID}
}

// --- PUT /me/sharing ----------------------------------------------------------------------------

type sharingRequest struct {
	ShareProgress *bool `json:"share_progress"`
}

func (h *Handlers) putSharing(w http.ResponseWriter, r *http.Request) {
	var req sharingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ShareProgress == nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "informe share_progress (true/false)"))
		return
	}
	u, err := h.q.SetShareProgress(r.Context(), dbgen.SetShareProgressParams{ID: currentUser(r).ID, Share: *req.ShareProgress})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	h.logger.Info("social: compartilhamento alterado", slog.String("user_id", uuidStr(u.ID)), slog.Bool("share", u.ShareProgress))
	writeJSON(w, http.StatusOK, toMeDTO(u))
}

// --- GET /social/leaderboard?period=month|year|all ----------------------------------------------

type leaderboardItemDTO struct {
	User         socialUserDTO `json:"user"`
	Platinum     int           `json:"platinum"`
	Earned       int           `json:"earned"`
	Rare         int           `json:"rare"`
	PsnPoints    int           `json:"psn_points"`
	Completed    int           `json:"completed"`
	LastUnlockAt *time.Time    `json:"last_unlock_at"`
}

type leaderboardDTO struct {
	Period string               `json:"period"`
	Items  []leaderboardItemDTO `json:"items"`
}

// periodStart: início do mês/ano corrente (UTC); "all" = sem corte.
func periodStart(period string, now time.Time) (pgtype.Timestamptz, bool) {
	now = now.UTC()
	switch period {
	case "", "all":
		return pgtype.Timestamptz{}, true
	case "month":
		return pgtype.Timestamptz{Time: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), Valid: true}, true
	case "year":
		return pgtype.Timestamptz{Time: time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}, true
	}
	return pgtype.Timestamptz{}, false
}

func (h *Handlers) getLeaderboard(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	since, ok := periodStart(period, time.Now())
	if !ok {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "period: month, year ou all"))
		return
	}
	if period == "" {
		period = "all"
	}
	rows, err := h.q.Leaderboard(r.Context(), since)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := leaderboardDTO{Period: period, Items: make([]leaderboardItemDTO, 0, len(rows))}
	for _, x := range rows {
		out.Items = append(out.Items, leaderboardItemDTO{
			User: h.socialUser(r, x.ID, x.Name), Platinum: int(x.Platinum), Earned: int(x.Earned), Rare: int(x.Rare),
			PsnPoints: int(x.PsnPoints), Completed: int(x.Completed), LastUnlockAt: tptr(x.LastUnlockAt),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- GET /social/feed ---------------------------------------------------------------------------

type feedItemDTO struct {
	User          socialUserDTO `json:"user"`
	Kind          string        `json:"kind"` // platinum | completion | rare
	TrophyID      string        `json:"trophy_id"`
	Name          string        `json:"name"`
	Description   *string       `json:"description"`
	Type          string        `json:"type"`
	Hidden        bool          `json:"hidden"`
	Icon          string        `json:"icon"`
	RarityPct     *float64      `json:"rarity_pct"`
	EarnedAt      *time.Time    `json:"earned_at"`
	GameTitle     string        `json:"game_title"`
	Platform      string        `json:"platform"`
	ExternalID    string        `json:"external_id"`
	Cover         string        `json:"cover"`
	Hero          string        `json:"hero"`
	CompletesGame bool          `json:"completes_game"`
}

type feedPageDTO struct {
	Items      []feedItemDTO `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}

func feedKind(row dbgen.SocialFeedRow) string {
	switch {
	case row.Type == "platinum":
		return "platinum"
	case row.CompletesGame:
		return "completion"
	default:
		return "rare"
	}
}

func (h *Handlers) getFeed(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	limit := clamp(atoiDefault(qp.Get("limit"), feedDefault), 1, feedMax)
	params := dbgen.SocialFeedParams{Lim: int32(limit)} //nolint:gosec // limit clampado
	if c := qp.Get("cursor"); c != "" {
		at, id, ok := parseCursor(c)
		if !ok {
			httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "cursor inválido"))
			return
		}
		params.BeforeAt, params.BeforeID = at, id
	}
	rows, err := h.q.SocialFeed(r.Context(), params)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := feedPageDTO{Items: make([]feedItemDTO, 0, len(rows))}
	for _, t := range rows {
		desc := t.Description
		if t.Hidden {
			desc = nil // troféu oculto: o nome a UI esconde até revelar; a descrição nem sai daqui
		}
		out.Items = append(out.Items, feedItemDTO{
			User: h.socialUser(r, t.UserID, t.UserName), Kind: feedKind(t),
			TrophyID: uuidStr(t.ID), Name: t.Name, Description: desc, Type: t.Type, Hidden: t.Hidden, Icon: t.Icon,
			RarityPct: t.RarityPct, EarnedAt: tptr(t.EarnedAt), GameTitle: t.GameTitle, Platform: t.Platform,
			ExternalID: t.ExternalID, Cover: t.Cover, Hero: t.Hero, CompletesGame: t.CompletesGame,
		})
	}
	if len(rows) == limit {
		last := rows[len(rows)-1]
		c := last.EarnedAt.Time.UTC().Format(time.RFC3339Nano) + "|" + uuidStr(last.ID)
		out.NextCursor = &c
	}
	writeJSON(w, http.StatusOK, out)
}

// --- GET /social/friends/{id} e /social/friends/{id}/games -------------------------------------

type friendProfileDTO struct {
	User  socialUserDTO `json:"user"`
	Stats statsDTO      `json:"stats"`
}

// loadFriend resolve {id} para um membro que compartilha; qualquer outro caso é 404.
func (h *Handlers) loadFriend(w http.ResponseWriter, r *http.Request) (dbgen.User, bool) {
	id, err := parseUUIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", ""))
		return dbgen.User{}, false
	}
	u, err := h.q.GetSharer(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", ""))
		return u, false
	}
	if err != nil {
		writeInternal(w, r, err)
		return u, false
	}
	return u, true
}

func (h *Handlers) getFriend(w http.ResponseWriter, r *http.Request) {
	f, ok := h.loadFriend(w, r)
	if !ok {
		return
	}
	stats, err := h.statsFor(r.Context(), f.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, friendProfileDTO{User: h.socialUser(r, f.ID, f.Name), Stats: stats})
}

func (h *Handlers) listFriendGames(w http.ResponseWriter, r *http.Request) {
	f, ok := h.loadFriend(w, r)
	if !ok {
		return
	}
	h.listGamesFor(w, r, f.ID)
}

// --- GET /games/{id}/friends --------------------------------------------------------------------

type gameFriendDTO struct {
	User          socialUserDTO `json:"user"`
	Platform      string        `json:"platform"`
	Earned        int           `json:"earned"`
	Total         int           `json:"total"`
	CompletionPct float64       `json:"completion_pct"`
	HasPlatinum   bool          `json:"has_platinum"`
	LastUnlockAt  *time.Time    `json:"last_unlock_at"`
}

type gameFriendsDTO struct {
	// Sharing: quem pede compartilha? false → a UI mostra o convite para ligar (items vazio).
	Sharing bool            `json:"sharing"`
	Items   []gameFriendDTO `json:"items"`
}

func (h *Handlers) getGameFriends(w http.ResponseWriter, r *http.Request) {
	g, ok := h.loadGameParam(w, r)
	if !ok {
		return
	}
	me := currentUser(r)
	out := gameFriendsDTO{Sharing: me.ShareProgress, Items: []gameFriendDTO{}}
	if !me.ShareProgress {
		writeJSON(w, http.StatusOK, out)
		return
	}
	rows, err := h.q.GameFriends(r.Context(), dbgen.GameFriendsParams{
		ViewerID: me.ID, Platform: g.Platform, ExternalID: g.ExternalID, NormalizedTitle: g.NormalizedTitle,
	})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	for _, x := range rows {
		out.Items = append(out.Items, gameFriendDTO{
			User: h.socialUser(r, x.UserID, x.UserName), Platform: x.Platform, Earned: int(x.Earned), Total: int(x.Total),
			CompletionPct: x.CompletionPct, HasPlatinum: x.HasPlatinum, LastUnlockAt: tptr(x.LastUnlockAt),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

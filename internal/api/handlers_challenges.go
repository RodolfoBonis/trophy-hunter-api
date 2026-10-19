package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/challenges"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
)

// Desafios entre amigos (rotas sob /social → exigem compartilhamento). Só participantes enxergam um
// desafio (senão 404). Convite: até 5 amigos que compartilham.

const maxChallengeFriends = 5

var allowedChallengeDays = map[int]bool{7: true, 14: true, 30: true}

type challengeGameDTO struct {
	Title      string `json:"title"`
	Platform   string `json:"platform"`
	ExternalID string `json:"external_id"`
	Cover      string `json:"cover"`
}

type challengeSummaryDTO struct {
	ID            string            `json:"id"`
	Kind          string            `json:"kind"`
	Title         string            `json:"title"`
	Status        string            `json:"status"`
	StartsAt      *time.Time        `json:"starts_at"`
	EndsAt        *time.Time        `json:"ends_at"`
	MyState       string            `json:"my_state"`
	IsCreator     bool              `json:"is_creator"`
	AcceptedCount int               `json:"accepted_count"`
	WinnerName    *string           `json:"winner_name"`
	Game          *challengeGameDTO `json:"game"`
}

type standingDTO struct {
	User          socialUserDTO `json:"user"`
	State         string        `json:"state"`
	Score         int           `json:"score"`
	HasGame       bool          `json:"has_game"`
	Earned        int           `json:"earned"`
	Total         int           `json:"total"`
	CompletionPct float64       `json:"completion_pct"`
	CompletedAt   *time.Time    `json:"completed_at"`
}

type challengeDetailDTO struct {
	challengeSummaryDTO
	FinishedAt *time.Time     `json:"finished_at"`
	Winner     *socialUserDTO `json:"winner"`
	Standings  []standingDTO  `json:"standings"`
}

func challengeGame(c dbgen.Challenge) *challengeGameDTO {
	if c.Kind != challenges.KindFirstToComplete || c.GameTitle == nil {
		return nil
	}
	cover := ""
	if c.GameCover != nil {
		cover = *c.GameCover
	}
	return &challengeGameDTO{Title: *c.GameTitle, Platform: derefStr(c.GamePlatform), ExternalID: derefStr(c.GameExternalID), Cover: cover}
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (h *Handlers) summaryOf(r *http.Request, c dbgen.Challenge, myState string, accepted int, winner *string) challengeSummaryDTO {
	return challengeSummaryDTO{
		ID: uuidStr(c.ID), Kind: c.Kind, Title: c.Title, Status: c.Status, StartsAt: tptr(c.StartsAt), EndsAt: tptr(c.EndsAt),
		MyState: myState, IsCreator: c.CreatorID == currentUser(r).ID, AcceptedCount: accepted, WinnerName: winner, Game: challengeGame(c),
	}
}

// --- GET /social/challenges ---------------------------------------------------------------------

func (h *Handlers) listChallenges(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListChallengesForUser(r.Context(), currentUser(r).ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]challengeSummaryDTO, 0, len(rows))
	for _, x := range rows {
		c := dbgen.Challenge{
			ID: x.ID, CreatorID: x.CreatorID, Kind: x.Kind, Title: x.Title, GamePlatform: x.GamePlatform, GameExternalID: x.GameExternalID,
			NormalizedTitle: x.NormalizedTitle, GameTitle: x.GameTitle, GameCover: x.GameCover, StartsAt: x.StartsAt, EndsAt: x.EndsAt,
			Status: x.Status, WinnerID: x.WinnerID, FinishedAt: x.FinishedAt, CreatedAt: x.CreatedAt,
		}
		out = append(out, h.summaryOf(r, c, x.MyState, int(x.AcceptedCount), x.WinnerName))
	}
	writeJSON(w, http.StatusOK, out)
}

// --- POST /social/challenges --------------------------------------------------------------------

type createChallengeRequest struct {
	Kind      string   `json:"kind"`
	FriendIDs []string `json:"friend_ids"`
	GameID    string   `json:"game_id"`
	Days      int      `json:"days"`
	Title     string   `json:"title"`
}

func (h *Handlers) createChallenge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := currentUser(r)
	var req createChallengeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo inválido"))
		return
	}
	bad := func(msg string) { httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", msg)) }
	if len(req.FriendIDs) == 0 || len(req.FriendIDs) > maxChallengeFriends {
		bad("convide de 1 a 5 amigos")
		return
	}
	friends := make([]dbgen.User, 0, len(req.FriendIDs))
	seen := map[pgtype.UUID]bool{}
	for _, raw := range req.FriendIDs {
		id, err := parseUUIDParam(raw)
		if err != nil || id == me.ID || seen[id] {
			bad("lista de amigos inválida")
			return
		}
		seen[id] = true
		f, err := h.q.GetSharer(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", "amigo não encontrado ou não compartilha"))
			return
		}
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		friends = append(friends, f)
	}

	p := dbgen.CreateChallengeParams{CreatorID: me.ID, Kind: req.Kind}
	days := req.Days
	switch req.Kind {
	case challenges.KindFirstToComplete:
		gid, err := parseUUIDParam(req.GameID)
		if err != nil {
			bad("escolha o jogo do desafio")
			return
		}
		g, err := h.q.GetGameForUser(ctx, dbgen.GetGameForUserParams{ID: gid, UserID: me.ID})
		if err != nil {
			writeNotFoundOr(w, r, err, "jogo não encontrado")
			return
		}
		prog, err := h.q.ChallengeGameProgress(ctx, dbgen.ChallengeGameProgressParams{UserID: me.ID, Platform: g.Platform, ExternalID: g.ExternalID, NormalizedTitle: g.NormalizedTitle})
		if err == nil && prog.Total > 0 && prog.Earned == prog.Total {
			httperr.Write(w, r, httperr.New(httperr.KindConflict, "Já concluído", "você já fechou esse jogo — escolha outro").WithCode("already_completed"))
			return
		}
		cover := g.Cover
		p.GamePlatform, p.GameExternalID, p.NormalizedTitle, p.GameTitle, p.GameCover = &g.Platform, &g.ExternalID, &g.NormalizedTitle, &g.Title, &cover
		goal := "a platina de "
		if g.Platform == "steam" {
			goal = "os 100% de "
		}
		p.Title = "Quem pega " + goal + g.Title + " primeiro"
		if days != 0 && !allowedChallengeDays[days] && days != 60 && days != 90 {
			bad("prazo: 7, 14, 30, 60 ou 90 dias (ou sem prazo)")
			return
		}
	case challenges.KindMostTrophies, challenges.KindMostRare:
		if days == 0 {
			days = 7
		}
		if !allowedChallengeDays[days] {
			bad("prazo: 7, 14 ou 30 dias")
			return
		}
		what := "troféus"
		if req.Kind == challenges.KindMostRare {
			what = "troféus raros"
		}
		p.Title = "Mais " + what + " em " + strconv.Itoa(days) + " dias"
	default:
		bad("tipo: first_to_complete, most_trophies ou most_rare")
		return
	}
	if t := strings.TrimSpace(req.Title); t != "" {
		if len(t) > 80 {
			t = t[:80]
		}
		p.Title = t
	}
	if days > 0 {
		p.EndsAt = pgtype.Timestamptz{Time: time.Now().Add(time.Duration(days) * 24 * time.Hour), Valid: true}
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := h.q.WithTx(tx)
	c, err := qtx.CreateChallenge(ctx, p)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if err := qtx.AddChallengeParticipant(ctx, dbgen.AddChallengeParticipantParams{ChallengeID: c.ID, UserID: me.ID, State: "accepted"}); err != nil {
		writeInternal(w, r, err)
		return
	}
	invited := make([]pgtype.UUID, 0, len(friends))
	for _, f := range friends {
		if err := qtx.AddChallengeParticipant(ctx, dbgen.AddChallengeParticipantParams{ChallengeID: c.ID, UserID: f.ID, State: "invited"}); err != nil {
			writeInternal(w, r, err)
			return
		}
		invited = append(invited, f.ID)
	}
	if err := tx.Commit(ctx); err != nil {
		writeInternal(w, r, err)
		return
	}
	h.challenges.NotifyInvite(ctx, c, me.Name, invited)
	h.logger.Info("desafio criado", slog.String("challenge_id", uuidStr(c.ID)), slog.String("kind", c.Kind), slog.Int("invited", len(invited)))
	h.writeChallenge(w, r, c, "accepted", http.StatusCreated)
}

// --- GET /social/challenges/{id} e ações --------------------------------------------------------

func (h *Handlers) loadChallenge(w http.ResponseWriter, r *http.Request) (dbgen.Challenge, string, bool) {
	id, err := parseUUIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", ""))
		return dbgen.Challenge{}, "", false
	}
	row, err := h.q.GetChallengeForUser(r.Context(), dbgen.GetChallengeForUserParams{ID: id, UserID: currentUser(r).ID})
	if err != nil {
		writeNotFoundOr(w, r, err, "desafio não encontrado")
		return dbgen.Challenge{}, "", false
	}
	c := dbgen.Challenge{
		ID: row.ID, CreatorID: row.CreatorID, Kind: row.Kind, Title: row.Title, GamePlatform: row.GamePlatform, GameExternalID: row.GameExternalID,
		NormalizedTitle: row.NormalizedTitle, GameTitle: row.GameTitle, GameCover: row.GameCover, StartsAt: row.StartsAt, EndsAt: row.EndsAt,
		Status: row.Status, WinnerID: row.WinnerID, FinishedAt: row.FinishedAt, CreatedAt: row.CreatedAt,
	}
	return c, row.MyState, true
}

func (h *Handlers) writeChallenge(w http.ResponseWriter, r *http.Request, c dbgen.Challenge, myState string, status int) {
	st, err := h.challenges.Standings(r.Context(), c)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	accepted := 0
	out := challengeDetailDTO{FinishedAt: tptr(c.FinishedAt), Standings: make([]standingDTO, 0, len(st))}
	for _, s := range st {
		if s.State == "accepted" {
			accepted++
		}
		su := h.socialUser(r, s.UserID, s.Name)
		if c.WinnerID.Valid && c.WinnerID == s.UserID {
			out.Winner = &su
		}
		out.Standings = append(out.Standings, standingDTO{
			User: su, State: s.State, Score: s.Score, HasGame: s.HasGame, Earned: s.Earned, Total: s.Total,
			CompletionPct: s.CompletionPct, CompletedAt: s.CompletedAt,
		})
	}
	var winnerName *string
	if out.Winner != nil {
		winnerName = &out.Winner.Name
	}
	out.challengeSummaryDTO = h.summaryOf(r, c, myState, accepted, winnerName)
	writeJSON(w, status, out)
}

func (h *Handlers) getChallenge(w http.ResponseWriter, r *http.Request) {
	c, state, ok := h.loadChallenge(w, r)
	if !ok {
		return
	}
	h.writeChallenge(w, r, c, state, http.StatusOK)
}

func (h *Handlers) respondChallenge(accept bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		c, state, ok := h.loadChallenge(w, r)
		if !ok {
			return
		}
		if c.Status != "open" || state != "invited" {
			httperr.Write(w, r, httperr.New(httperr.KindConflict, "Conflito", "esse convite não está mais pendente"))
			return
		}
		me := currentUser(r)
		if accept && c.Kind == challenges.KindFirstToComplete {
			prog, err := h.q.ChallengeGameProgress(ctx, dbgen.ChallengeGameProgressParams{
				UserID: me.ID, Platform: derefStr(c.GamePlatform), ExternalID: derefStr(c.GameExternalID), NormalizedTitle: derefStr(c.NormalizedTitle),
			})
			if err == nil && prog.Total > 0 && prog.Earned == prog.Total && prog.LastUnlockAt.Time.Before(c.StartsAt.Time) {
				httperr.Write(w, r, httperr.New(httperr.KindConflict, "Já concluído", "você já tinha fechado esse jogo antes do desafio").WithCode("already_completed"))
				return
			}
		}
		newState := "declined"
		if accept {
			newState = "accepted"
		}
		if _, err := h.q.RespondChallenge(ctx, dbgen.RespondChallengeParams{ChallengeID: c.ID, UserID: me.ID, State: newState}); err != nil {
			writeNotFoundOr(w, r, err, "convite não encontrado")
			return
		}
		h.writeChallenge(w, r, c, newState, http.StatusOK)
	}
}

func (h *Handlers) cancelChallenge(w http.ResponseWriter, r *http.Request) {
	c, state, ok := h.loadChallenge(w, r)
	if !ok {
		return
	}
	n, err := h.q.CancelChallenge(r.Context(), dbgen.CancelChallengeParams{ID: c.ID, CreatorID: currentUser(r).ID})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if n == 0 {
		httperr.Write(w, r, httperr.New(httperr.KindConflict, "Conflito", "só quem criou cancela, e só desafio em andamento"))
		return
	}
	c.Status = "canceled"
	h.writeChallenge(w, r, c, state, http.StatusOK)
}

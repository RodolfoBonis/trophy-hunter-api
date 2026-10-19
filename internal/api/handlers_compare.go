package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
)

// GET /games/{id}/compare/{friendId}: troféu a troféu do MEU jogo contra a versão mais avançada do
// mesmo jogo de um amigo que compartilha (mesma regra recíproca do social). Mesma plataforma → casa
// pelo id do troféu; plataformas diferentes (Steam × PSN) → pelo nome (melhor esforço).

type compareTrophyDTO struct {
	TrophyID       string     `json:"trophy_id"`
	Name           string     `json:"name"`
	Description    *string    `json:"description"`
	Type           string     `json:"type"`
	Hidden         bool       `json:"hidden"`
	Icon           string     `json:"icon"`
	RarityPct      *float64   `json:"rarity_pct"`
	MineEarnedAt   *time.Time `json:"mine_earned_at"`
	Mine           bool       `json:"mine"`
	Friend         bool       `json:"friend"`
	FriendEarnedAt *time.Time `json:"friend_earned_at"`
	// Matched = o troféu existe na versão do amigo (Steam × PSN podem ter listas diferentes).
	Matched bool `json:"matched"`
}

type compareDTO struct {
	Friend         socialUserDTO      `json:"friend"`
	SamePlatform   bool               `json:"same_platform"`
	FriendPlatform string             `json:"friend_platform"`
	FriendEarned   int                `json:"friend_earned"`
	FriendTotal    int                `json:"friend_total"`
	FriendPct      float64            `json:"friend_completion_pct"`
	Trophies       []compareTrophyDTO `json:"trophies"`
}

func trophyNameKey(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

func (h *Handlers) compareGame(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !currentUser(r).ShareProgress {
		writeSharingDisabled(w, r)
		return
	}
	g, ok := h.loadGameParam(w, r)
	if !ok {
		return
	}
	fid, err := parseUUIDParam(chi.URLParam(r, "friendId"))
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", ""))
		return
	}
	friend, err := h.q.GetSharer(ctx, fid)
	if err != nil {
		writeNotFoundOr(w, r, err, "amigo não encontrado")
		return
	}
	fg, err := h.q.FriendGameMatch(ctx, dbgen.FriendGameMatchParams{UserID: friend.ID, Platform: g.Platform, ExternalID: g.ExternalID, NormalizedTitle: g.NormalizedTitle})
	if errors.Is(err, pgx.ErrNoRows) {
		httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", "esse amigo não tem este jogo"))
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	mine, err := h.q.ListTrophiesByGame(ctx, g.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	theirs, err := h.q.ListTrophiesByGame(ctx, fg.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	same := fg.Platform == g.Platform && fg.ExternalID == g.ExternalID
	byKey := make(map[string]dbgen.Trophy, len(theirs))
	for _, t := range theirs {
		k := t.ExternalID
		if !same {
			k = trophyNameKey(t.Name)
		}
		byKey[k] = t
	}
	out := compareDTO{
		Friend: h.socialUser(r, friend.ID, friend.Name), SamePlatform: same, FriendPlatform: fg.Platform,
		FriendEarned: int(fg.Earned), FriendTotal: int(fg.Total), FriendPct: fg.CompletionPct,
		Trophies: make([]compareTrophyDTO, 0, len(mine)),
	}
	for _, t := range mine {
		k := t.ExternalID
		if !same {
			k = trophyNameKey(t.Name)
		}
		desc := t.Description
		if t.Hidden && !t.Earned {
			desc = nil // spoiler: descrição de oculto que eu não tenho não sai
		}
		item := compareTrophyDTO{
			TrophyID: uuidStr(t.ID), Name: t.Name, Description: desc, Type: t.Type, Hidden: t.Hidden, Icon: t.Icon,
			RarityPct: t.RarityPct, Mine: t.Earned, MineEarnedAt: tptr(t.EarnedAt),
		}
		if ft, ok := byKey[k]; ok {
			item.Matched, item.Friend, item.FriendEarnedAt = true, ft.Earned, tptr(ft.EarnedAt)
		}
		out.Trophies = append(out.Trophies, item)
	}
	writeJSON(w, http.StatusOK, out)
}

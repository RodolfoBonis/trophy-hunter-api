package api

import (
	"time"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/progress"
)

// userDTO ↔ components.schemas.User
type userDTO struct {
	Subject string `json:"subject"`
	Email   string `json:"email,omitempty"`
	Name    string `json:"name,omitempty"`
}

// integrationDTO ↔ components.schemas.Integration. NUNCA inclui credenciais.
type integrationDTO struct {
	Platform     string     `json:"platform"`
	Status       string     `json:"status"`
	AccountID    string     `json:"account_id"`
	DisplayName  string     `json:"display_name"`
	Avatar       string     `json:"avatar"`
	LastSyncedAt *time.Time `json:"last_synced_at"`
	LastError    string     `json:"last_error"`
}

func toIntegrationDTO(i dbgen.Integration) integrationDTO {
	return integrationDTO{
		Platform:     i.Platform,
		Status:       i.Status,
		AccountID:    i.AccountID,
		DisplayName:  i.DisplayName,
		Avatar:       i.Avatar,
		LastSyncedAt: tptr(i.LastSyncedAt),
		LastError:    i.LastError,
	}
}

// gameDTO ↔ components.schemas.Game
type gameDTO struct {
	ID              string     `json:"id"`
	Platform        string     `json:"platform"`
	ExternalID      string     `json:"external_id"`
	NpServiceName   *string    `json:"np_service_name"`
	Title           string     `json:"title"`
	Icon            string     `json:"icon"`
	Cover           string     `json:"cover"`
	Platforms       []string   `json:"platforms"`
	HasAchievements bool       `json:"has_achievements"`
	HasPlatinum     bool       `json:"has_platinum"`
	LastPlayedAt    *time.Time `json:"last_played_at"`
	PlaytimeMinutes int        `json:"playtime_minutes"`
	Abandoned       bool       `json:"abandoned"`
	Priority        int        `json:"priority"`
	Notes           string     `json:"notes"`
	Tags            []string   `json:"tags"`
}

func toGameDTO(g dbgen.Game) gameDTO {
	return gameDTO{
		ID:              uuidStr(g.ID),
		Platform:        g.Platform,
		ExternalID:      g.ExternalID,
		NpServiceName:   g.NpServiceName,
		Title:           g.Title,
		Icon:            g.Icon,
		Cover:           g.Cover,
		Platforms:       nonNil(g.Platforms),
		HasAchievements: g.HasAchievements,
		HasPlatinum:     g.HasPlatinum,
		LastPlayedAt:    tptr(g.LastPlayedAt),
		PlaytimeMinutes: int(g.PlaytimeMinutes),
		Abandoned:       g.Abandoned,
		Priority:        int(g.Priority),
		Notes:           g.Notes,
		Tags:            nonNil(g.Tags),
	}
}

// progressSummaryDTO ↔ components.schemas.ProgressSummary (versão compacta para a lista).
type progressSummaryDTO struct {
	Total         int     `json:"total"`
	Earned        int     `json:"earned"`
	Remaining     int     `json:"remaining"`
	CompletionPct float64 `json:"completion_pct"`
	Status        string  `json:"status"`
	HasPlatinum   bool    `json:"has_platinum"`
}

// gameListItemDTO ↔ components.schemas.GameListItem (Game + progress).
type gameListItemDTO struct {
	gameDTO
	Progress progressSummaryDTO `json:"progress"`
}

func toListItemDTO(r dbgen.ListGamesRow) gameListItemDTO {
	g := gameDTO{
		ID:              uuidStr(r.ID),
		Platform:        r.Platform,
		ExternalID:      r.ExternalID,
		NpServiceName:   r.NpServiceName,
		Title:           r.Title,
		Icon:            r.Icon,
		Cover:           r.Cover,
		Platforms:       nonNil(r.Platforms),
		HasAchievements: r.HasAchievements,
		HasPlatinum:     r.HasPlatinum,
		LastPlayedAt:    tptr(r.LastPlayedAt),
		PlaytimeMinutes: int(r.PlaytimeMinutes),
		Abandoned:       r.Abandoned,
		Priority:        int(r.Priority),
		Notes:           r.Notes,
		Tags:            nonNil(r.Tags),
	}
	return gameListItemDTO{
		gameDTO: g,
		Progress: progressSummaryDTO{
			Total:         int(r.Total),
			Earned:        int(r.Earned),
			Remaining:     int(r.Remaining),
			CompletionPct: r.CompletionPct,
			Status:        r.StatusBucket,
			HasPlatinum:   r.HasPlatinum,
		},
	}
}

// gameListPageDTO ↔ components.schemas.GameListPage
type gameListPageDTO struct {
	Items  []gameListItemDTO `json:"items"`
	Total  int               `json:"total"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}

// trophyDTO ↔ components.schemas.Trophy
type trophyDTO struct {
	ID                 string     `json:"id"`
	ExternalID         string     `json:"external_id"`
	Name               string     `json:"name"`
	Description        *string    `json:"description"`
	Type               string     `json:"type"`
	Hidden             bool       `json:"hidden"`
	Icon               string     `json:"icon"`
	IconLocked         string     `json:"icon_locked"`
	RarityPct          *float64   `json:"rarity_pct"`
	Earned             bool       `json:"earned"`
	EarnedAt           *time.Time `json:"earned_at"`
	Progress           *int       `json:"progress"`
	Target             *int       `json:"target"`
	Priority           int        `json:"priority"`
	Notes              string     `json:"notes"`
	Tags               []string   `json:"tags"`
	ServerShutdownRisk bool       `json:"server_shutdown_risk"`
}

func toTrophyDTO(t dbgen.Trophy) trophyDTO {
	return trophyDTO{
		ID:                 uuidStr(t.ID),
		ExternalID:         t.ExternalID,
		Name:               t.Name,
		Description:        t.Description,
		Type:               t.Type,
		Hidden:             t.Hidden,
		Icon:               t.Icon,
		IconLocked:         t.IconLocked,
		RarityPct:          t.RarityPct,
		Earned:             t.Earned,
		EarnedAt:           tptr(t.EarnedAt),
		Progress:           intPtr(t.Progress),
		Target:             intPtr(t.Target),
		Priority:           int(t.Priority),
		Notes:              t.Notes,
		Tags:               nonNil(t.Tags),
		ServerShutdownRisk: t.ServerShutdownRisk,
	}
}

// trophyGroupDTO ↔ components.schemas.TrophyGroup
type trophyGroupDTO struct {
	ID         string      `json:"id"`
	ExternalID string      `json:"external_id"`
	Name       string      `json:"name"`
	IsBase     bool        `json:"is_base"`
	Icon       string      `json:"icon"`
	Trophies   []trophyDTO `json:"trophies"`
}

// gameDetailDTO ↔ components.schemas.GameDetail
type gameDetailDTO struct {
	gameDTO
	Progress progress.Summary `json:"progress"`
	Groups   []trophyGroupDTO `json:"groups"`
}

// syncRunDTO ↔ components.schemas.SyncRun
//
// games_total/games_done/games_failed refletem o fan-out: total de jogos a sincronizar no run,
// quantos terminaram e quantos falharam. (games_touched foi substituído por esses campos.)
type syncRunDTO struct {
	ID           string     `json:"id"`
	Platform     string     `json:"platform"`
	Scope        string     `json:"scope"`
	Trigger      string     `json:"trigger"`
	Status       string     `json:"status"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	ItemsChanged int        `json:"items_changed"`
	GamesTotal   int        `json:"games_total"`
	GamesDone    int        `json:"games_done"`
	GamesFailed  int        `json:"games_failed"`
	Error        string     `json:"error"`
}

func toSyncRunDTO(r dbgen.SyncRun) syncRunDTO {
	return syncRunDTO{
		ID:           uuidStr(r.ID),
		Platform:     r.Platform,
		Scope:        r.Scope,
		Trigger:      r.Trigger,
		Status:       r.Status,
		StartedAt:    r.StartedAt.Time,
		FinishedAt:   tptr(r.FinishedAt),
		ItemsChanged: int(r.ItemsChanged),
		GamesTotal:   int(r.GamesTotal),
		GamesDone:    int(r.GamesDone),
		GamesFailed:  int(r.GamesFailed),
		Error:        r.Error,
	}
}

// enqueueResponseDTO ↔ components.schemas.EnqueueResponse
type enqueueResponseDTO struct {
	Enqueued bool    `json:"enqueued"`
	JobID    *string `json:"job_id"`
}

func intPtr(p *int32) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}

// nonNil garante que listas saiam como [] no JSON (nunca null): o contrato declara arrays e o
// front valida com zod — um slice nil vira null e quebra a validação da página inteira.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

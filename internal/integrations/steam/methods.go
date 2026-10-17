package steam

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

// GetOwnedGames lista a biblioteca do jogador (com appinfo e jogos free já jogados).
func (c *Client) GetOwnedGames(ctx context.Context, steamID string) ([]OwnedGame, error) {
	q := url.Values{}
	q.Set("key", c.apiKey)
	q.Set("steamid", steamID)
	q.Set("include_appinfo", "1")
	q.Set("include_played_free_games", "1")
	q.Set("format", "json")

	var raw ownedGamesResp
	if err := c.doJSON(ctx, "GetOwnedGames", "/IPlayerService/GetOwnedGames/v1/", q, &raw); err != nil {
		return nil, err
	}
	games := make([]OwnedGame, 0, len(raw.Response.Games))
	for _, g := range raw.Response.Games {
		games = append(games, OwnedGame{
			AppID:                    g.AppID,
			Name:                     g.Name,
			PlaytimeForever:          g.PlaytimeForever,
			RtimeLastPlayed:          g.RtimeLastPlayed,
			ImgIconURL:               g.ImgIconURL,
			HasCommunityVisibleStats: g.HasCommunityVisibleStats,
		})
	}
	return games, nil
}

// GetPlayerAchievements devolve o estado das conquistas do jogador para um app.
// Mapeia os erros de domínio (perfil privado, sem stats) a partir do campo error da resposta.
func (c *Client) GetPlayerAchievements(ctx context.Context, steamID string, appID int) ([]PlayerAchievement, error) {
	q := url.Values{}
	q.Set("key", c.apiKey)
	q.Set("steamid", steamID)
	q.Set("appid", strconv.Itoa(appID))
	q.Set("l", c.lang)

	var raw playerAchievementsResp
	if err := c.doJSON(ctx, "GetPlayerAchievements", "/ISteamUserStats/GetPlayerAchievements/v1/", q, &raw); err != nil {
		return nil, err
	}
	if !raw.PlayerStats.Success {
		return nil, classifyStatsError(raw.PlayerStats.Error)
	}
	out := make([]PlayerAchievement, 0, len(raw.PlayerStats.Achievements))
	for _, a := range raw.PlayerStats.Achievements {
		out = append(out, PlayerAchievement{
			APIName:    a.APIName,
			Achieved:   a.Achieved == 1,
			UnlockTime: a.UnlockTime,
		})
	}
	return out, nil
}

func classifyStatsError(msg string) error {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "not public"):
		return ErrProfilePrivate
	case strings.Contains(m, "no stats"):
		return ErrNoStats
	default:
		return ErrNoStats // sem sucesso e sem erro reconhecido: tratamos como sem stats
	}
}

// GetSchemaForGame devolve a definição das conquistas (nome, oculta, descrição, ícones).
// Ocultas sem descrição vêm com Description == nil (spoiler guard).
func (c *Client) GetSchemaForGame(ctx context.Context, appID int) (gameName string, achievements []SchemaAchievement, err error) {
	q := url.Values{}
	q.Set("key", c.apiKey)
	q.Set("appid", strconv.Itoa(appID))
	q.Set("l", c.lang)

	var raw schemaResp
	if err := c.doJSON(ctx, "GetSchemaForGame", "/ISteamUserStats/GetSchemaForGame/v2/", q, &raw); err != nil {
		return "", nil, err
	}
	defs := raw.Game.AvailableGameStats.Achievements
	out := make([]SchemaAchievement, 0, len(defs))
	for _, a := range defs {
		out = append(out, SchemaAchievement{
			Name:        a.Name,
			DisplayName: a.DisplayName,
			Hidden:      a.Hidden == 1,
			Description: a.Description,
			Icon:        a.Icon,
			IconGray:    a.IconGray,
		})
	}
	return raw.Game.GameName, out, nil
}

// GetGlobalAchievementPercentages devolve o percentual global por conquista (name → percent).
// Tolera percent vindo como número ou como string.
func (c *Client) GetGlobalAchievementPercentages(ctx context.Context, appID int) (map[string]float64, error) {
	q := url.Values{}
	q.Set("gameid", strconv.Itoa(appID))
	q.Set("format", "json")

	var raw globalPercentagesResp
	if err := c.doJSON(ctx, "GetGlobalAchievementPercentagesForApp",
		"/ISteamUserStats/GetGlobalAchievementPercentagesForApp/v2/", q, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(raw.AchievementPercentages.Achievements))
	for _, a := range raw.AchievementPercentages.Achievements {
		out[a.Name] = float64(a.Percent)
	}
	return out, nil
}

// GetPlayerSummary devolve o resumo de um perfil (validação de conexão).
func (c *Client) GetPlayerSummary(ctx context.Context, steamID string) (PlayerSummary, error) {
	q := url.Values{}
	q.Set("key", c.apiKey)
	q.Set("steamids", steamID)

	var raw playerSummariesResp
	if err := c.doJSON(ctx, "GetPlayerSummaries", "/ISteamUser/GetPlayerSummaries/v2/", q, &raw); err != nil {
		return PlayerSummary{}, err
	}
	if len(raw.Response.Players) == 0 {
		return PlayerSummary{}, ErrProfilePrivate
	}
	p := raw.Response.Players[0]
	return PlayerSummary{
		SteamID:               p.SteamID,
		PersonaName:           p.PersonaName,
		AvatarFull:            p.AvatarFull,
		CommunityVisibleState: p.CommunityVisibilityState,
	}, nil
}

// ResolveVanityURL resolve uma vanity URL para um SteamID64.
func (c *Client) ResolveVanityURL(ctx context.Context, vanity string) (string, error) {
	q := url.Values{}
	q.Set("key", c.apiKey)
	q.Set("vanityurl", vanity)

	var raw resolveVanityResp
	if err := c.doJSON(ctx, "ResolveVanityURL", "/ISteamUser/ResolveVanityURL/v1/", q, &raw); err != nil {
		return "", err
	}
	if raw.Response.Success != 1 || raw.Response.SteamID == "" {
		return "", ErrVanityNotFound
	}
	return raw.Response.SteamID, nil
}

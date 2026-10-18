package steam

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// OwnedGame é um jogo da biblioteca (normalizado de GetOwnedGames).
type OwnedGame struct {
	AppID                    int
	Name                     string
	PlaytimeForever          int   // minutos
	RtimeLastPlayed          int64 // unix seconds (0 = nunca)
	ImgIconURL               string
	HasCommunityVisibleStats bool
	// NotOwned: o jogo veio de "jogados recentemente", não da biblioteca (Família Steam/empréstimo,
	// fim de semana gratuito). Sem rtime_last_played nem has_community_visible_stats na API.
	NotOwned bool
}

// PlayerAchievement é o estado de uma conquista para o jogador (GetPlayerAchievements).
type PlayerAchievement struct {
	APIName    string
	Achieved   bool
	UnlockTime int64 // unix seconds (0 = não desbloqueada)
}

// SchemaAchievement é a definição de uma conquista (GetSchemaForGame).
// Description é nil quando ausente (comum em conquistas ocultas → spoiler guard).
type SchemaAchievement struct {
	Name        string
	DisplayName string
	Hidden      bool
	Description *string
	Icon        string
	IconGray    string
}

// PlayerSummary é o resumo do perfil (GetPlayerSummaries), usado na validação de conexão.
type PlayerSummary struct {
	SteamID               string
	PersonaName           string
	AvatarFull            string
	CommunityVisibleState int // 3 = público
}

// IsPublic indica visibilidade pública do perfil (communityvisibilitystate == 3).
func (p PlayerSummary) IsPublic() bool { return p.CommunityVisibleState == 3 }

// --- respostas cruas da API ---

type recentGamesResp struct {
	Response struct {
		Games []struct {
			AppID           int    `json:"appid"`
			Name            string `json:"name"`
			PlaytimeForever int    `json:"playtime_forever"`
			ImgIconURL      string `json:"img_icon_url"`
		} `json:"games"`
	} `json:"response"`
}

type ownedGamesResp struct {
	Response struct {
		GameCount int `json:"game_count"`
		Games     []struct {
			AppID                    int    `json:"appid"`
			Name                     string `json:"name"`
			PlaytimeForever          int    `json:"playtime_forever"`
			RtimeLastPlayed          int64  `json:"rtime_last_played"`
			ImgIconURL               string `json:"img_icon_url"`
			HasCommunityVisibleStats bool   `json:"has_community_visible_stats"`
		} `json:"games"`
	} `json:"response"`
}

type playerAchievementsResp struct {
	PlayerStats struct {
		SteamID      string `json:"steamID"`
		GameName     string `json:"gameName"`
		Success      bool   `json:"success"`
		Error        string `json:"error"`
		Achievements []struct {
			APIName    string `json:"apiname"`
			Achieved   int    `json:"achieved"`
			UnlockTime int64  `json:"unlocktime"`
		} `json:"achievements"`
	} `json:"playerstats"`
}

type schemaResp struct {
	Game struct {
		GameName           string `json:"gameName"`
		AvailableGameStats struct {
			Achievements []struct {
				Name        string  `json:"name"`
				DisplayName string  `json:"displayName"`
				Hidden      int     `json:"hidden"`
				Description *string `json:"description"`
				Icon        string  `json:"icon"`
				IconGray    string  `json:"icongray"`
			} `json:"achievements"`
		} `json:"availableGameStats"`
	} `json:"game"`
}

type globalPercentagesResp struct {
	AchievementPercentages struct {
		Achievements []struct {
			Name    string    `json:"name"`
			Percent flexFloat `json:"percent"`
		} `json:"achievements"`
	} `json:"achievementpercentages"`
}

type playerSummariesResp struct {
	Response struct {
		Players []struct {
			SteamID                  string `json:"steamid"`
			PersonaName              string `json:"personaname"`
			AvatarFull               string `json:"avatarfull"`
			CommunityVisibilityState int    `json:"communityvisibilitystate"`
		} `json:"players"`
	} `json:"response"`
}

type resolveVanityResp struct {
	Response struct {
		Success int    `json:"success"`
		SteamID string `json:"steamid"`
		Message string `json:"message"`
	} `json:"response"`
}

// flexFloat decodifica um número que pode vir como número JSON OU como string (a Steam alterna
// entre os dois em GetGlobalAchievementPercentagesForApp).
type flexFloat float64

// UnmarshalJSON aceita 3.14 e "3.14".
func (f *flexFloat) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		*f = 0
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			*f = 0
			return nil
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("percent string inválido %q: %w", s, err)
		}
		*f = flexFloat(v)
		return nil
	}
	var v float64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*f = flexFloat(v)
	return nil
}

// storeItemsResp é o recorte usado de IStoreBrowseService/GetItems (include_assets=true).
type storeItemsResp struct {
	Response struct {
		StoreItems []struct {
			AppID  int `json:"appid"`
			Assets struct {
				AssetURLFormat string `json:"asset_url_format"`
				LibraryCapsule string `json:"library_capsule"`
				Header         string `json:"header"`
			} `json:"assets"`
		} `json:"store_items"`
	} `json:"response"`
}

package psn_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/psn"
)

func newClient(t *testing.T, h http.Handler) *psn.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return psn.New(psn.Options{AuthBaseURL: srv.URL + "/auth", APIBaseURL: srv.URL + "/api", RPS: 1000, Burst: 1000})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func authHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/authorize":
			if r.Header.Get("Cookie") != "npsso=GOOD" {
				w.Header().Set("Location", "com.scee.psxandroid.scecompcall://redirect/?error=login_required")
				w.WriteHeader(http.StatusFound)
				return
			}
			assert.Equal(t, "offline", r.URL.Query().Get("access_type"))
			w.Header().Set("Location", "com.scee.psxandroid.scecompcall://redirect/?code=v3.CODE&cid=x")
			w.WriteHeader(http.StatusFound)
		case "/auth/token":
			require.NoError(t, r.ParseForm())
			assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Basic "))
			switch r.Form.Get("grant_type") {
			case "authorization_code":
				assert.Equal(t, "v3.CODE", r.Form.Get("code"))
				writeJSON(w, map[string]any{"access_token": "ACC", "expires_in": 3600, "refresh_token": "REF", "refresh_token_expires_in": 5184000})
			case "refresh_token":
				if r.Form.Get("refresh_token") != "REF" {
					w.WriteHeader(http.StatusBadRequest)
					writeJSON(w, map[string]any{"error": "invalid_grant"})
					return
				}
				writeJSON(w, map[string]any{"access_token": "ACC2", "expires_in": 3600, "refresh_token": "REF2", "refresh_token_expires_in": 5184000})
			}
		default:
			http.NotFound(w, r)
		}
	}
}

func TestExchangeAndRefresh(t *testing.T) {
	c := newClient(t, authHandler(t))
	ctx := context.Background()

	tok, err := c.ExchangeNPSSO(ctx, " GOOD ")
	require.NoError(t, err)
	assert.Equal(t, "ACC", tok.AccessToken)
	assert.Equal(t, "REF", tok.RefreshToken)
	assert.WithinDuration(t, time.Now().Add(time.Hour), tok.AccessExpiresAt, 5*time.Second)
	assert.WithinDuration(t, time.Now().Add(60*24*time.Hour), tok.RefreshExpiresAt, 5*time.Second)

	_, err = c.ExchangeNPSSO(ctx, "BAD")
	require.ErrorIs(t, err, psn.ErrInvalidNPSSO)
	_, err = c.ExchangeNPSSO(ctx, "  ")
	require.ErrorIs(t, err, psn.ErrInvalidNPSSO)

	tok2, err := c.Refresh(ctx, "REF")
	require.NoError(t, err)
	assert.Equal(t, "ACC2", tok2.AccessToken)
	_, err = c.Refresh(ctx, "EXPIRED")
	require.ErrorIs(t, err, psn.ErrInvalidGrant)
}

func TestTitlesPaginationAndHeaders(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer ACC", r.Header.Get("Authorization"))
		assert.Equal(t, "pt-BR", r.Header.Get("Accept-Language"))
		require.Equal(t, "/api/trophy/v1/users/me/trophyTitles", r.URL.Path)
		if r.URL.Query().Get("offset") == "0" {
			writeJSON(w, map[string]any{"trophyTitles": []map[string]any{
				{"npCommunicationId": "NPWR1_00", "npServiceName": "trophy2", "trophyTitleName": "Astro Bot", "trophyTitlePlatform": "PS5",
					"definedTrophies": map[string]int{"platinum": 1, "gold": 4}, "lastUpdatedDateTime": "2026-10-12T21:47:00Z"},
			}, "totalItemCount": 2, "nextOffset": 1})
			return
		}
		writeJSON(w, map[string]any{"trophyTitles": []map[string]any{{"npCommunicationId": "NPWR2_00", "npServiceName": "trophy", "trophyTitleName": "Bloodborne"}}, "totalItemCount": 2})
	}))
	titles, err := c.TrophyTitles(context.Background(), "ACC")
	require.NoError(t, err)
	require.Len(t, titles, 2)
	assert.Equal(t, "Astro Bot", titles[0].TrophyTitleName)
	assert.Equal(t, 1, titles[0].DefinedTrophies.Platinum)
	assert.Equal(t, "trophy2", titles[0].NpServiceName)
	assert.Equal(t, 2026, titles[0].LastUpdatedDateTime.Year())
	assert.Equal(t, "NPWR2_00", titles[1].NpCommunicationID)
}

func TestTrophiesGroupsAndUserState(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "trophy2", r.URL.Query().Get("npServiceName"))
		switch r.URL.Path {
		case "/api/trophy/v1/npCommunicationIds/NPWR1_00/trophyGroups/all/trophies":
			writeJSON(w, map[string]any{"trophies": []map[string]any{
				{"trophyId": 0, "trophyType": "platinum", "trophyName": "Platina", "trophyDetail": "Tudo", "trophyIconUrl": "https://i/p.png", "trophyGroupId": "default"},
				{"trophyId": 1, "trophyType": "bronze", "trophyHidden": true, "trophyName": "Segredo", "trophyGroupId": "001", "trophyProgressTargetValue": "120"},
			}})
		case "/api/trophy/v1/npCommunicationIds/NPWR1_00/trophyGroups":
			writeJSON(w, map[string]any{"trophyGroups": []map[string]any{{"trophyGroupId": "default", "trophyGroupName": "Astro Bot"}, {"trophyGroupId": "001", "trophyGroupName": "DLC"}}})
		case "/api/trophy/v1/users/me/npCommunicationIds/NPWR1_00/trophyGroups/all/trophies":
			writeJSON(w, map[string]any{"trophies": []map[string]any{
				{"trophyId": 0, "earned": true, "earnedDateTime": "2026-10-12T21:47:00Z", "trophyEarnedRate": "3.1"},
				{"trophyId": 1, "earned": false, "trophyEarnedRate": "", "progress": "87"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	ctx := context.Background()
	defs, err := c.TitleTrophies(ctx, "ACC", "NPWR1_00", "trophy2")
	require.NoError(t, err)
	require.Len(t, defs, 2)
	assert.True(t, defs[1].TrophyHidden)
	assert.Equal(t, "120", defs[1].TrophyProgressTargetValue)
	groups, err := c.TitleGroups(ctx, "ACC", "NPWR1_00", "trophy2")
	require.NoError(t, err)
	require.Len(t, groups, 2)
	user, err := c.UserTrophies(ctx, "ACC", "NPWR1_00", "trophy2")
	require.NoError(t, err)
	assert.True(t, user[0].Earned)
	rate, ok := psn.ParseRate(user[0].TrophyEarnedRate)
	assert.True(t, ok)
	assert.InDelta(t, 3.1, rate, 1e-9)
	_, ok = psn.ParseRate(user[1].TrophyEarnedRate)
	assert.False(t, ok)
	assert.Equal(t, "87", user[1].Progress)
}

func TestErrorsAndRetry(t *testing.T) {
	var calls atomic.Int32
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/trophy/v1/users/me/trophySummary":
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			writeJSON(w, map[string]any{"accountId": "123", "trophyLevel": 421})
		case "/api/trophy/v1/users/me/trophyTitles":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	ctx := context.Background()
	start := time.Now()
	s, err := c.TrophySummary(ctx, "SECRET-TOKEN")
	require.NoError(t, err)
	assert.Equal(t, "123", s.AccountID)
	assert.Equal(t, 421, s.TrophyLevel)
	assert.GreaterOrEqual(t, time.Since(start), 900*time.Millisecond, "respeita Retry-After")

	_, err = c.TrophyTitles(ctx, "SECRET-TOKEN")
	require.ErrorIs(t, err, psn.ErrUnauthorized)
	_, err = c.TitleGroups(ctx, "SECRET-TOKEN", "NOPE", "")
	require.ErrorIs(t, err, psn.ErrNotFound)
	assert.NotContains(t, err.Error(), "SECRET-TOKEN")
}

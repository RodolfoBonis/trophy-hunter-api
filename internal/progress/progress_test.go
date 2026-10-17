package progress_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/progress"
)

func ach(earned, base bool) progress.Trophy {
	return progress.Trophy{Type: progress.TypeAchievement, Earned: earned, IsBase: base}
}

func TestCompute(t *testing.T) {
	tests := []struct {
		name          string
		trophies      []progress.Trophy
		abandoned     bool
		cfg           progress.Config
		wantTotal     int
		wantEarned    int
		wantRemaining int
		wantStatus    string
		wantPct       float64
		wantBaseTotal int
		wantDLCTotal  int
		wantPlatinum  bool
	}{
		{
			name:       "jogo vazio",
			trophies:   nil,
			wantStatus: progress.StatusNotStarted,
		},
		{
			name:          "sem progresso",
			trophies:      []progress.Trophy{ach(false, true), ach(false, true)},
			wantTotal:     2,
			wantRemaining: 2,
			wantBaseTotal: 2,
			wantStatus:    progress.StatusNotStarted,
		},
		{
			name:          "em andamento",
			trophies:      []progress.Trophy{ach(true, true), ach(false, true), ach(false, true), ach(false, true)},
			wantTotal:     4,
			wantEarned:    1,
			wantRemaining: 3,
			wantPct:       0.25,
			wantBaseTotal: 4,
			wantStatus:    progress.StatusInProgress,
		},
		{
			name:          "quase lá no limiar default 80%",
			trophies:      []progress.Trophy{ach(true, true), ach(true, true), ach(true, true), ach(true, true), ach(false, true)},
			wantTotal:     5,
			wantEarned:    4,
			wantRemaining: 1,
			wantPct:       0.8,
			wantBaseTotal: 5,
			wantStatus:    progress.StatusAlmost,
		},
		{
			name:          "completo 100% sem platina (Steam)",
			trophies:      []progress.Trophy{ach(true, true), ach(true, true)},
			wantTotal:     2,
			wantEarned:    2,
			wantPct:       1.0,
			wantBaseTotal: 2,
			wantStatus:    progress.StatusCompleted,
		},
		{
			name: "platinado (PSN)",
			trophies: []progress.Trophy{
				{Type: progress.TypePlatinum, Earned: true, IsBase: true},
				{Type: progress.TypeGold, Earned: true, IsBase: true},
			},
			wantTotal:     2,
			wantEarned:    2,
			wantPct:       1.0,
			wantBaseTotal: 2,
			wantPlatinum:  true,
			wantStatus:    progress.StatusPlatinum,
		},
		{
			name:          "abandonado vence tudo",
			trophies:      []progress.Trophy{ach(true, true), ach(true, true)},
			abandoned:     true,
			wantTotal:     2,
			wantEarned:    2,
			wantPct:       1.0,
			wantBaseTotal: 2,
			wantStatus:    progress.StatusAbandoned,
		},
		{
			name: "só DLC",
			trophies: []progress.Trophy{
				ach(true, false), ach(false, false),
			},
			wantTotal:     2,
			wantEarned:    1,
			wantRemaining: 1,
			wantPct:       0.5,
			wantDLCTotal:  2,
			wantStatus:    progress.StatusInProgress,
		},
		{
			name:          "limiar custom 50%",
			trophies:      []progress.Trophy{ach(true, true), ach(false, true)},
			cfg:           progress.Config{AlmostThreshold: 0.5},
			wantTotal:     2,
			wantEarned:    1,
			wantRemaining: 1,
			wantPct:       0.5,
			wantBaseTotal: 2,
			wantStatus:    progress.StatusAlmost,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := progress.Compute(tt.trophies, tt.abandoned, tt.cfg)
			require.Equal(t, tt.wantTotal, s.Total)
			require.Equal(t, tt.wantEarned, s.Earned)
			require.Equal(t, tt.wantRemaining, s.Remaining)
			require.InDelta(t, tt.wantPct, s.CompletionPct, 0.0001)
			require.Equal(t, tt.wantStatus, s.Status)
			require.Equal(t, tt.wantBaseTotal, s.Base.Total)
			require.Equal(t, tt.wantDLCTotal, s.DLC.Total)
			require.Equal(t, tt.wantPlatinum, s.HasPlatinum)
		})
	}
}

func TestByTypeBreakdown(t *testing.T) {
	trophies := []progress.Trophy{
		{Type: progress.TypePlatinum, Earned: false, IsBase: true},
		{Type: progress.TypeGold, Earned: true, IsBase: true},
		{Type: progress.TypeGold, Earned: false, IsBase: true},
		{Type: progress.TypeBronze, Earned: true, IsBase: true},
	}
	s := progress.Compute(trophies, false, progress.Config{})
	require.Equal(t, progress.TypeCount{Total: 1, Earned: 0}, s.ByType[progress.TypePlatinum])
	require.Equal(t, progress.TypeCount{Total: 2, Earned: 1}, s.ByType[progress.TypeGold])
	require.Equal(t, progress.TypeCount{Total: 1, Earned: 1}, s.ByType[progress.TypeBronze])
	require.True(t, s.HasPlatinum)
	require.False(t, s.PlatinumEarned)
}

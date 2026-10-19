package recap

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func d(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, Location) }

func TestParsePeriod(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, Location)
	p, err := ParsePeriod("", now)
	require.NoError(t, err)
	require.Equal(t, "2026-10", p.Key)
	require.Equal(t, "outubro de 2026", p.Label)
	require.True(t, p.Partial)
	require.Equal(t, d(2026, 9, 1), p.PrevSince)

	y, err := ParsePeriod("2025", now)
	require.NoError(t, err)
	require.Equal(t, d(2025, 1, 1), y.Since)
	require.Equal(t, d(2026, 1, 1), y.Until)
	require.False(t, y.Partial)

	for _, bad := range []string{"2026-13", "abc", "1999", "2026-x"} {
		_, err := ParsePeriod(bad, now)
		require.Error(t, err, bad)
	}
}

func TestStreakAndWeekday(t *testing.T) {
	days := []time.Time{d(2026, 10, 1), d(2026, 10, 2), d(2026, 10, 3), d(2026, 10, 7), d(2026, 10, 8)}
	require.Equal(t, 3, LongestStreak(days))
	require.Zero(t, LongestStreak(nil))
	// 3/out/2026 é sábado: com mais desbloqueios ele vence.
	require.Equal(t, "sábado", FavoriteWeekday(days, []int{1, 1, 9, 1, 1}))
	require.Equal(t, "", FavoriteWeekday(nil, nil))
}

func TestWeekStartAndDue(t *testing.T) {
	sun := time.Date(2026, 10, 18, 19, 0, 0, 0, Location) // domingo 19h
	require.Equal(t, d(2026, 10, 12), WeekStart(sun))
	require.True(t, WeeklyDue(sun))
	require.False(t, WeeklyDue(sun.Add(-2*time.Hour)))
	require.Equal(t, d(2026, 10, 19), WeekStart(time.Date(2026, 10, 19, 0, 30, 0, 0, Location)))
}

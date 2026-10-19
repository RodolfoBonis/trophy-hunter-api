package challenges

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"
)

func uid(b byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{b}, Valid: true} }

var start = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func TestFirstToCompleteEarliestWins(t *testing.T) {
	st := []Standing{
		{UserID: uid(1), State: "accepted", HasGame: true, Earned: 10, Total: 10, LastUnlockAt: start.Add(48 * time.Hour)},
		{UserID: uid(2), State: "accepted", HasGame: true, Earned: 10, Total: 10, LastUnlockAt: start.Add(24 * time.Hour)},
		{UserID: uid(3), State: "invited", HasGame: true, Earned: 10, Total: 10, LastUnlockAt: start.Add(time.Hour)}, // não aceitou
	}
	d := Decide(KindFirstToComplete, start, nil, start.Add(72*time.Hour), st)
	require.True(t, d.Finish)
	require.Equal(t, uid(2), *d.Winner)
}

func TestFirstToCompleteIgnoresCompletionBeforeStart(t *testing.T) {
	st := []Standing{{UserID: uid(1), State: "accepted", HasGame: true, Earned: 5, Total: 5, LastUnlockAt: start.Add(-time.Hour)}}
	require.False(t, Decide(KindFirstToComplete, start, nil, start.Add(time.Hour), st).Finish)
}

func TestFirstToCompleteDeadlineWithoutWinner(t *testing.T) {
	end := start.Add(7 * 24 * time.Hour)
	st := []Standing{{UserID: uid(1), State: "accepted", HasGame: true, Earned: 4, Total: 5, LastUnlockAt: start.Add(time.Hour)}}
	d := Decide(KindFirstToComplete, start, &end, end, st)
	require.True(t, d.Finish)
	require.Nil(t, d.Winner)
}

func TestMostTrophiesOnlyAtDeadlineAndTieHasNoWinner(t *testing.T) {
	end := start.Add(7 * 24 * time.Hour)
	st := []Standing{{UserID: uid(1), State: "accepted", Score: 30}, {UserID: uid(2), State: "accepted", Score: 12}}
	require.False(t, Decide(KindMostTrophies, start, &end, end.Add(-time.Minute), st).Finish, "antes do prazo não termina")
	d := Decide(KindMostTrophies, start, &end, end, st)
	require.Equal(t, uid(1), *d.Winner)

	st[1].Score = 30
	d = Decide(KindMostRare, start, &end, end, st)
	require.True(t, d.Finish)
	require.Nil(t, d.Winner, "empate no topo")
}

func TestNextMilestone(t *testing.T) {
	require.Equal(t, 50, NextMilestone(0.55, 0))
	require.Equal(t, 90, NextMilestone(0.95, 50), "pula direto para o maior atingido")
	require.Zero(t, NextMilestone(0.6, 50))
}

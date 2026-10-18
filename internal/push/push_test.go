package push

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
)

func gid(b byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{b}, Valid: true} }

func TestUnlockMessages(t *testing.T) {
	plat := dbgen.ListUnpushedUnlocksRow{TrophyName: "Pai e filho", GameID: gid(1), GameTitle: "God of War", Platform: "psn", CompletesGame: true}
	full := dbgen.ListUnpushedUnlocksRow{TrophyName: "Fim", GameID: gid(2), GameTitle: "Portal 2", Platform: "steam", CompletesGame: true}
	one := dbgen.ListUnpushedUnlocksRow{TrophyName: "Segredo", GameID: gid(3), GameTitle: "Hades", Platform: "steam"}

	msgs := unlockMessages([]dbgen.ListUnpushedUnlocksRow{plat, full}, []dbgen.ListUnpushedUnlocksRow{one})
	require.Len(t, msgs, 3)
	require.Equal(t, Message{Kind: KindCompletions, Title: "Platina! God of War", Body: "Pai e filho", URL: "/games/01000000-0000-0000-0000-000000000000", Tag: "completion-01000000-0000-0000-0000-000000000000"}, msgs[0])
	require.Equal(t, "100%! Portal 2", msgs[1].Title)
	require.Equal(t, "Conquista desbloqueada: Segredo", msgs[2].Title)
	require.Equal(t, "Hades", msgs[2].Body)

	var many []dbgen.ListUnpushedUnlocksRow
	for i, g := range []string{"A", "B", "C", "D", "A"} {
		many = append(many, dbgen.ListUnpushedUnlocksRow{TrophyName: "t", GameID: gid(byte(10 + map[string]int{"A": 0, "B": 1, "C": 2, "D": 3}[g])), GameTitle: g, Platform: "psn"})
		_ = i
	}
	agg := unlockMessages(nil, many)
	require.Len(t, agg, 1)
	require.Equal(t, "5 troféus novos", agg[0].Title)
	require.Equal(t, "A, B, C e mais 1", agg[0].Body)
	require.Equal(t, "/", agg[0].URL, "vários jogos → abre o início")
}

func TestPreferencesAllows(t *testing.T) {
	p := DefaultPreferences()
	for _, k := range []Kind{KindUnlocks, KindCompletions, KindReconnect, KindAIReady, KindTest} {
		require.True(t, p.Allows(k), k)
	}
	p.AIReady = false
	require.False(t, p.Allows(KindAIReady))
	require.True(t, p.Allows(KindTest), "teste ignora preferências")
}

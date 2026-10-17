package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
)

// Regressão: jogos da Steam não têm platforms/tags; slices nil viravam `null` no JSON e o zod do
// front rejeitava a página inteira da biblioteca. O contrato exige arrays (vazios), nunca null.
func TestDTOListsMarshalAsEmptyArrays(t *testing.T) {
	cases := map[string]any{
		"game":      toGameDTO(dbgen.Game{}),
		"list item": toListItemDTO(dbgen.ListGamesRow{}),
		"trophy":    toTrophyDTO(dbgen.Trophy{}),
	}
	for name, dto := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(dto)
			require.NoError(t, err)
			var m map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &m))
			for _, key := range []string{"platforms", "tags"} {
				v, ok := m[key]
				if !ok {
					continue // trophy não tem platforms
				}
				assert.JSONEq(t, "[]", string(v), "%s.%s deve ser [] e não null", name, key)
			}
			assert.Contains(t, m, "tags")
		})
	}
}

package ai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
)

const guideOK = `{"summary":"S","steps":["a"," ",""],"prerequisites":[],"missable":true,"point_of_no_return":"Cap. 7",
"difficulty":5,"estimated_time":"1 h","tips":[],"confidence":0.4,"uncertainty_notes":"talvez","sources":[]}`

func TestValidateGuide(t *testing.T) {
	out, err := ValidateGuide(ScopeTrophy, []byte(guideOK))
	require.NoError(t, err)
	var g TrophyGuide
	require.NoError(t, json.Unmarshal(out, &g))
	require.Equal(t, []string{"a"}, g.Steps, "itens vazios são removidos")

	cases := map[string]string{
		"campo extra":      strings.Replace(guideOK, `"summary":"S"`, `"summary":"S","x":1`, 1),
		"campo ausente":    strings.Replace(guideOK, `"missable":true,`, ``, 1),
		"dificuldade":      strings.Replace(guideOK, `"difficulty":5`, `"difficulty":11`, 1),
		"confiança":        strings.Replace(guideOK, `"confidence":0.4`, `"confidence":1.5`, 1),
		"sem passos":       strings.Replace(guideOK, `["a"," ",""]`, `[]`, 1),
		"null obrigatório": strings.Replace(guideOK, `"tips":[]`, `"tips":null`, 1),
		"não é objeto":     `[]`,
	}
	for name, raw := range cases {
		_, err := ValidateGuide(ScopeTrophy, []byte(raw))
		require.ErrorIs(t, err, ErrInvalidGuide, name)
	}
	_, err = ValidateGuide("outro", []byte(guideOK))
	require.ErrorIs(t, err, ErrInvalidGuide)

	_, err = ValidateGuide(ScopeRoadmap, []byte(`{"summary":"S","difficulty":2,"estimated_total_time":"1h","playthroughs":0,
"stages":[{"title":"t","description":"d","trophies":[],"missables":[]}],"missables_overview":[],"tips":[],"confidence":1,"uncertainty_notes":"","sources":[]}`))
	require.ErrorIs(t, err, ErrInvalidGuide, "playthroughs ≥ 1")
}

func TestSchemasAreValidJSON(t *testing.T) {
	for _, s := range []string{TrophyGuideSchema, RoadmapSchema} {
		require.True(t, json.Valid([]byte(s)))
	}
}

func TestProgressContextHidesHiddenDescriptions(t *testing.T) {
	d := "Derrote o pai do protagonista"
	ctx := progressContext(dbgen.Game{Title: "Jogo", Platform: "psn"}, []dbgen.GameTrophiesForAIRow{
		{Name: "Revelação", Type: "gold", Hidden: true, Description: &d, IsBase: true},
		{Name: "Feito", Type: "bronze", Earned: true, IsBase: true},
		{Name: "Extra", Type: "silver", IsBase: false, GroupName: "Ilha", Tags: []string{"missable"}},
	})
	require.Contains(t, ctx, "Progresso: 1 de 3")
	require.Contains(t, ctx, "Revelação (ouro, raridade desconhecida, oculto)")
	require.NotContains(t, ctx, d, "descrição de oculto não vai para o prompt")
	require.NotContains(t, ctx, "- Feito", "o que já foi obtido não entra na lista do que falta")
	require.Contains(t, ctx, "DLC Ilha, marcado: missable")
}

func TestChatPromptTranscriptLimit(t *testing.T) {
	var hist []chatTurn
	for i := 0; i < 30; i++ {
		hist = append(hist, chatTurn{Role: "user", Content: "msg" + strings.Repeat("x", i)})
	}
	p := chatPrompt(dbgen.Game{Title: "J"}, nil, hist, "pergunta", false)
	require.NotContains(t, p, "Eu: msg\n", "só as últimas mensagens")
	require.Contains(t, p, "Minha pergunta: pergunta")
	resumed := chatPrompt(dbgen.Game{Title: "J"}, nil, hist, "pergunta", true)
	require.NotContains(t, resumed, "Conversa até agora", "com --resume a CLI já tem o histórico")
}

func TestValidateGuideWhereIsOptionalForV1(t *testing.T) {
	out, err := ValidateGuide(ScopeTrophy, []byte(strings.Replace(guideOK, `"summary":"S"`, `"where":"Capítulo 3","summary":"S"`, 1)))
	require.NoError(t, err)
	require.Contains(t, string(out), `"where":"Capítulo 3"`)
	_, err = ValidateGuide(ScopeTrophy, []byte(guideOK))
	require.NoError(t, err, "guias v1 (sem where) continuam válidos")
	require.Contains(t, TrophyGuideSchema, `"required":["where"`, "o modelo é obrigado a preencher where")
}

func TestValidateRecap(t *testing.T) {
	ok := `{"headline":" Boa semana ","highlights":["a",""," b "],"suggestions":[{"game":"Hades","why":"curto","trophies":[""]}],"encouragement":"vai"}`
	out, err := ValidateRecap([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `""`) {
		t.Fatalf("itens vazios deveriam sair: %s", out)
	}
	for _, bad := range []string{`{"headline":"","highlights":[],"suggestions":[],"encouragement":""}`, `{"headline":"x"}`, `{"headline":"x","highlights":[],"suggestions":[],"encouragement":"","extra":1}`} {
		if _, err := ValidateRecap([]byte(bad)); err == nil {
			t.Fatalf("deveria rejeitar: %s", bad)
		}
	}
}

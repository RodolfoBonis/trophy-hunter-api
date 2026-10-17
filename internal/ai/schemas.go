// Package ai gera guias por troféu, roadmap de platina e respostas do chat usando a CLI do Claude
// Code (ver internal/ai/claude). A saída dos guias é estruturada: o JSON Schema vai para a CLI
// (--json-schema) e o resultado é revalidado aqui antes de ir para o banco — a mesma validação vale
// para a edição feita pelo usuário.
package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// SchemaVersion é a versão dos schemas de guia (ai_guides.schema_version).
// v2: campo "where" no guia de troféu (opcional na validação, para guias v1 continuarem válidos).
const SchemaVersion = 2

// TrophyGuide é o guia de um troféu (schema trophy_guide v1).
type TrophyGuide struct {
	Where            string   `json:"where"`
	Summary          string   `json:"summary"`
	Steps            []string `json:"steps"`
	Prerequisites    []string `json:"prerequisites"`
	Missable         bool     `json:"missable"`
	PointOfNoReturn  string   `json:"point_of_no_return"`
	Difficulty       int      `json:"difficulty"`
	EstimatedTime    string   `json:"estimated_time"`
	Tips             []string `json:"tips"`
	Confidence       float64  `json:"confidence"`
	UncertaintyNotes string   `json:"uncertainty_notes"`
	Sources          []string `json:"sources"`
}

// RoadmapStage é uma etapa do roadmap.
type RoadmapStage struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Trophies    []string `json:"trophies"`
	Missables   []string `json:"missables"`
}

// Roadmap é o plano de platina/100% de um jogo (schema roadmap v1).
type Roadmap struct {
	Summary            string         `json:"summary"`
	Difficulty         int            `json:"difficulty"`
	EstimatedTotalTime string         `json:"estimated_total_time"`
	Playthroughs       int            `json:"playthroughs"`
	Stages             []RoadmapStage `json:"stages"`
	MissablesOverview  []string       `json:"missables_overview"`
	Tips               []string       `json:"tips"`
	Confidence         float64        `json:"confidence"`
	UncertaintyNotes   string         `json:"uncertainty_notes"`
	Sources            []string       `json:"sources"`
}

const strList = `{"type":"array","items":{"type":"string"}}`

// TrophyGuideSchema é o JSON Schema passado à CLI para o guia de troféu.
var TrophyGuideSchema = compact(`{
  "type":"object","additionalProperties":false,
  "required":["where","summary","steps","prerequisites","missable","point_of_no_return","difficulty","estimated_time","tips","confidence","uncertainty_notes","sources"],
  "properties":{
    "where":{"type":"string","description":"Onde o troféu sai, curto: capítulo/fase/área (nome em PT-BR; original entre parênteses se diferente). Ex.: \"Capítulo 3 — Esperanças da Primavera (Hopes of Spring), fase O Sublaboratório\". Vazio se não depende de lugar."},
    "summary":{"type":"string","description":"1–2 frases curtas dizendo O QUE fazer. Sem ressalvas nem fontes."},
    "steps":{"type":"array","minItems":1,"maxItems":8,"items":{"type":"string"},"description":"Passos em ordem, cada um uma ação objetiva no imperativo (1 frase, no máximo 2). Sem 'as fontes divergem', sem hipóteses."},
    "prerequisites":{"type":"array","maxItems":4,"items":{"type":"string"},"description":"O que precisa antes (curto). Vazio se nada."},
    "missable":{"type":"boolean","description":"Pode ser perdido (exige outro save/NG+ se passar do ponto)."},
    "point_of_no_return":{"type":"string","description":"Se perdível: a partir de onde não dá mais. Vazio se não for perdível."},
    "difficulty":{"type":"integer","minimum":1,"maximum":10},
    "estimated_time":{"type":"string","description":"Ex.: \"10 min\", \"2–3 h\"."},
    "tips":{"type":"array","maxItems":4,"items":{"type":"string"},"description":"Dicas práticas de jogo (não notas sobre fontes). Vazio se nada útil."},
    "confidence":{"type":"number","minimum":0,"maximum":1,"description":"Confiança na precisão do guia."},
    "uncertainty_notes":{"type":"string","description":"UMA frase para o jogador sobre o que conferir no jogo; vazio se confiante. Não narre a pesquisa."},
    "sources":{"type":"array","items":{"type":"string"},"description":"URLs consultadas."}
  }}`)

// RoadmapSchema é o JSON Schema passado à CLI para o roadmap.
var RoadmapSchema = compact(`{
  "type":"object","additionalProperties":false,
  "required":["summary","difficulty","estimated_total_time","playthroughs","stages","missables_overview","tips","confidence","uncertainty_notes","sources"],
  "properties":{
    "summary":{"type":"string","description":"2–3 frases: o plano em resumo. Sem ressalvas."},
    "difficulty":{"type":"integer","minimum":1,"maximum":10},
    "estimated_total_time":{"type":"string","description":"Curto. Ex.: \"12–18 h\"."},
    "playthroughs":{"type":"integer","minimum":1,"description":"Mínimo de jogadas necessárias."},
    "stages":{"type":"array","minItems":1,"items":{
      "type":"object","additionalProperties":false,"required":["title","description","trophies","missables"],
      "properties":{
        "title":{"type":"string","description":"Curto, sem numeração (a UI numera)."},
        "description":{"type":"string","description":"O que fazer nesta etapa, em 2–4 frases objetivas."},
        "trophies":{"type":"array","items":{"type":"string"},"description":"Nomes EXATOS dos troféus desta etapa, como na lista fornecida."},
        "missables":{"type":"array","items":{"type":"string"},"description":"Perdíveis a garantir antes de avançar (curto). Vazio se nenhum."}
      }}},
    "missables_overview":{"type":"array","maxItems":5,"items":{"type":"string"},"description":"Avisos importantes do jogo inteiro (perdíveis, co-op, etc.), curtos."},
    "tips":{"type":"array","maxItems":5,"items":{"type":"string"},"description":"Dicas práticas de jogo."},
    "confidence":{"type":"number","minimum":0,"maximum":1},
    "uncertainty_notes":{"type":"string","description":"UMA frase para o jogador sobre o que conferir; vazio se confiante. Não narre a pesquisa."},
    "sources":` + strList + `
  }}`)

// ErrInvalidGuide indica conteúdo que não segue o schema.
var ErrInvalidGuide = errors.New("guia fora do schema")

// ValidateGuide valida e normaliza o JSON de um guia do escopo dado (trophy|roadmap). Rejeita
// campos desconhecidos/ausentes e valores fora da faixa; devolve o JSON canônico para gravar.
func ValidateGuide(scope string, raw []byte) ([]byte, error) {
	switch scope {
	case ScopeTrophy:
		var g TrophyGuide
		if err := strictDecode(raw, &g, TrophyGuideSchema); err != nil {
			return nil, err
		}
		if strings.TrimSpace(g.Summary) == "" || len(g.Steps) == 0 {
			return nil, fmt.Errorf("%w: summary e steps são obrigatórios", ErrInvalidGuide)
		}
		if err := checkRanges(g.Difficulty, g.Confidence); err != nil {
			return nil, err
		}
		g.Steps, g.Prerequisites, g.Tips, g.Sources = clean(g.Steps), clean(g.Prerequisites), clean(g.Tips), clean(g.Sources)
		return json.Marshal(g)
	case ScopeRoadmap:
		var r Roadmap
		if err := strictDecode(raw, &r, RoadmapSchema); err != nil {
			return nil, err
		}
		if strings.TrimSpace(r.Summary) == "" || len(r.Stages) == 0 {
			return nil, fmt.Errorf("%w: summary e stages são obrigatórios", ErrInvalidGuide)
		}
		if r.Playthroughs < 1 {
			return nil, fmt.Errorf("%w: playthroughs deve ser ≥ 1", ErrInvalidGuide)
		}
		if err := checkRanges(r.Difficulty, r.Confidence); err != nil {
			return nil, err
		}
		for i := range r.Stages {
			r.Stages[i].Trophies, r.Stages[i].Missables = clean(r.Stages[i].Trophies), clean(r.Stages[i].Missables)
		}
		r.MissablesOverview, r.Tips, r.Sources = clean(r.MissablesOverview), clean(r.Tips), clean(r.Sources)
		return json.Marshal(r)
	default:
		return nil, fmt.Errorf("%w: escopo %q", ErrInvalidGuide, scope)
	}
}

// strictDecode decodifica rejeitando campos desconhecidos e exige que todos os campos do schema
// estejam presentes (o "required" do JSON Schema — o zero value do Go não distingue ausência).
func strictDecode(raw []byte, dst any, schema string) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidGuide, err)
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal(raw, &present); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidGuide, err)
	}
	var s struct {
		Required []string `json:"required"`
	}
	_ = json.Unmarshal([]byte(schema), &s)
	for _, k := range s.Required {
		if optionalOnRead[k] {
			continue
		}
		if v, ok := present[k]; !ok || string(v) == "null" {
			return fmt.Errorf("%w: campo %q ausente", ErrInvalidGuide, k)
		}
	}
	return nil
}

// optionalOnRead são campos exigidos do modelo mas opcionais na validação (guias de versões
// anteriores não os têm).
var optionalOnRead = map[string]bool{"where": true}

func checkRanges(difficulty int, confidence float64) error {
	if difficulty < 1 || difficulty > 10 {
		return fmt.Errorf("%w: difficulty deve estar entre 1 e 10", ErrInvalidGuide)
	}
	if confidence < 0 || confidence > 1 {
		return fmt.Errorf("%w: confidence deve estar entre 0 e 1", ErrInvalidGuide)
	}
	return nil
}

// clean remove itens vazios e garante [] (nunca null) no JSON.
func clean(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func compact(s string) string {
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		panic("schema inválido: " + err.Error())
	}
	return b.String()
}

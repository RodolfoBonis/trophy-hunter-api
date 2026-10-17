//go:build manual

package ai

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/ai/claude"
)

// Smoke manual com a CLI real: go test -tags manual -run TestRealGuide ./internal/ai/ (precisa do token).
func TestRealGuide(t *testing.T) {
	r := claude.New("claude", os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"), t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	d := "Obtenha todos os outros troféus."
	rar := 2.3
	prompt := trophyGuidePrompt(dbgen.GetTrophyWithGameRow{Name: "Is It Over?", Type: "platinum", Description: &d, RarityPct: &rar,
		GameTitle: "Hades", GamePlatform: "psn", IsBase: true})
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	n := map[claude.EventType]int{}
	res, err := r.Run(ctx, claude.Request{Prompt: prompt, SystemPrompt: systemPrompt, Model: "sonnet", JSONSchema: TrophyGuideSchema,
		AllowedTools: AllowedTools, Workdir: t.TempDir()}, func(e claude.Event) {
		n[e.Type]++
		if e.Type == claude.EventTool {
			t.Log(e.Text)
		}
	})
	if err != nil || res.Err != "" {
		t.Fatalf("err=%v res.Err=%s", err, res.Err)
	}
	out, verr := ValidateGuide(ScopeTrophy, res.Structured)
	if verr != nil {
		t.Fatalf("validação: %v\n%s", verr, res.Structured)
	}
	t.Logf("events=%v cost=%.3f\n%s", n, res.CostUSD, out)
}

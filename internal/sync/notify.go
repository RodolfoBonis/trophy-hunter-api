package sync

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/push"
)

// Notificações disparadas pelo sync (entregues pelo worker via job `notify`; ver internal/push).

const integrationsURL = "/settings/integrations"

// enqueueUnlocksNotice pede o push dos desbloqueios novos (o job agrega tudo que ainda não foi
// notificado; dedupe evita um job por run quando várias contas fecham juntas).
func (s *Syncer) enqueueUnlocksNotice(ctx context.Context) {
	if err := push.Enqueue(ctx, s.queue, push.Payload{Kind: push.KindUnlocks}, "notify:unlocks"); err != nil {
		s.logger.Warn("push: enfileirar desbloqueios falhou", slog.String("err", err.Error()))
	}
}

// notifyStatus avisa o usuário quando a conta precisa de AÇÃO dele (reautenticar a PSN, deixar o
// perfil Steam público). Erro transitório de sync não é assunto do usuário: vira alerta de operação
// (Telegram) pelas métricas. ClaimStatusNotification garante um aviso por transição.
func (s *Syncer) notifyStatus(ctx context.Context, integID pgtype.UUID, status string) {
	if status == "connected" {
		if err := s.q.MarkStatusConnected(ctx, integID); err != nil {
			s.logger.Warn("push: marcar conta conectada falhou", slog.String("err", err.Error()))
		}
		return
	}
	if status != "needs_reconnect" && status != "error_private" {
		return
	}
	row, err := s.q.ClaimStatusNotification(ctx, dbgen.ClaimStatusNotificationParams{ID: integID, Status: status})
	if errors.Is(err, pgx.ErrNoRows) {
		return // já notificado (ou dentro da janela anti-spam)
	}
	if err != nil {
		s.logger.Warn("push: registrar aviso de conta falhou", slog.String("err", err.Error()))
		return
	}
	if err := push.Enqueue(ctx, s.queue, statusPayload(row, status), ""); err != nil {
		s.logger.Warn("push: enfileirar aviso de conta falhou", slog.String("err", err.Error()))
	}
}

// statusPayload monta o texto do aviso de conta. Separada para teste.
func statusPayload(row dbgen.ClaimStatusNotificationRow, status string) push.Payload {
	platform := "Steam"
	if row.Platform == "psn" {
		platform = "PSN"
	}
	account := platform
	if row.Label != "" {
		account += " (" + row.Label + ")"
	}
	tag := "account-" + row.Platform
	switch status {
	case "needs_reconnect":
		body := "A sessão expirou. Abra Contas e cole um NPSSO novo."
		if row.Platform == "steam" {
			body = "A conta precisa ser reconectada em Contas."
		}
		return push.Payload{UserID: push.UserIDString(row.UserID), Kind: push.KindReconnect, Title: "Reconecte a conta " + account, Body: body, URL: integrationsURL, Tag: tag}
	default: // error_private
		return push.Payload{UserID: push.UserIDString(row.UserID), Kind: push.KindReconnect, Title: "Perfil " + account + " ficou privado",
			Body: "Deixe os detalhes do jogo públicos na Steam para o sync voltar a funcionar.", URL: integrationsURL, Tag: tag}
	}
}

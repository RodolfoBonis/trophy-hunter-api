-- name: ListWishlist :many
SELECT w.id, w.platform, w.store_id, w.title, w.cover, w.target_price_cents, w.added_at,
       p.status AS price_status, p.price_cents, p.regular_cents, p.cut, p.low_cents, p.shop, p.url AS price_url, p.fetched_at AS price_fetched_at,
       e.completionist_h, e.main_h
FROM wishlist w
LEFT JOIN game_prices p ON p.platform = w.platform AND p.store_id = w.store_id
LEFT JOIN game_estimates e ON e.normalized_title = w.normalized_title AND e.status = 'found'
WHERE w.user_id = $1
ORDER BY w.added_at DESC;

-- name: AddWishlist :one
INSERT INTO wishlist (user_id, platform, store_id, title, normalized_title, cover, target_price_cents)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (user_id, platform, store_id) DO UPDATE SET target_price_cents = EXCLUDED.target_price_cents
RETURNING id;

-- name: SetWishlistTarget :execrows
UPDATE wishlist SET target_price_cents = sqlc.narg(target), notified_price_cents = NULL
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);

-- name: DeleteWishlist :execrows
DELETE FROM wishlist WHERE id = $1 AND user_id = $2;

-- name: OwnedNormalizedTitles :many
-- Quais destes títulos a pessoa já tem (a busca marca "na biblioteca").
SELECT DISTINCT normalized_title FROM games WHERE user_id = $1 AND normalized_title = ANY(sqlc.arg(titles)::text[]);

-- name: StaleWishlistPrices :many
-- Itens sem preço ou com preço velho (12 h; erro tenta de novo em 1 h).
SELECT w.platform, w.store_id, MIN(w.title)::text AS title
FROM wishlist w
LEFT JOIN game_prices p ON p.platform = w.platform AND p.store_id = w.store_id
WHERE p.store_id IS NULL
   OR (p.status <> 'error' AND p.fetched_at < now() - interval '12 hours')
   OR (p.status = 'error' AND p.fetched_at < now() - interval '1 hour')
GROUP BY w.platform, w.store_id, p.store_id
ORDER BY (p.store_id IS NULL) DESC
LIMIT sqlc.arg(lim);

-- name: UpsertGamePrice :exec
INSERT INTO game_prices (platform, store_id, status, currency, price_cents, regular_cents, cut, low_cents, shop, url, fetched_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
ON CONFLICT (platform, store_id) DO UPDATE SET
    status = EXCLUDED.status, currency = EXCLUDED.currency, price_cents = EXCLUDED.price_cents,
    regular_cents = EXCLUDED.regular_cents, cut = EXCLUDED.cut, low_cents = EXCLUDED.low_cents,
    shop = EXCLUDED.shop, url = EXCLUDED.url, fetched_at = now();

-- name: TouchGamePriceError :exec
-- Falha ao buscar: mantém o último preço bom, só marca o erro (tenta de novo em 1 h).
INSERT INTO game_prices (platform, store_id, status, fetched_at) VALUES ($1, $2, 'error', now())
ON CONFLICT (platform, store_id) DO UPDATE SET status = 'error', fetched_at = now();

-- name: DealsToNotify :many
-- Promoções que valem aviso: preço ≤ alvo da pessoa, ou no menor preço histórico com desconto. Só
-- avisa de novo se o preço baixar mais que o último avisado.
SELECT w.id, w.user_id, w.title, w.platform, p.price_cents, p.cut, p.shop, (p.low_cents IS NOT NULL AND p.price_cents <= p.low_cents)::boolean AS historic_low
FROM wishlist w
JOIN game_prices p ON p.platform = w.platform AND p.store_id = w.store_id
WHERE p.status = 'ok' AND p.price_cents IS NOT NULL
  AND ((w.target_price_cents IS NOT NULL AND p.price_cents <= w.target_price_cents)
       OR (p.cut > 0 AND p.low_cents IS NOT NULL AND p.price_cents <= p.low_cents))
  AND (w.notified_price_cents IS NULL OR p.price_cents < w.notified_price_cents);

-- name: MarkWishlistNotified :exec
UPDATE wishlist SET notified_price_cents = $2 WHERE id = $1;

-- +goose Up
-- A promo code worth 0 is an access code: it reveals hidden tickets at full
-- price. Go refuses 0 unless the code unlocks something; the database keeps
-- the upper bounds and refuses anything below 0.
-- 00010 left the value CHECK unnamed, so Postgres called it promo_codes_check.
ALTER TABLE promo_codes DROP CONSTRAINT promo_codes_check;
ALTER TABLE promo_codes ADD CONSTRAINT promo_codes_value_check
  CHECK ((kind = 'percent' AND value BETWEEN 0 AND 10000) OR (kind = 'amount' AND value BETWEEN 0 AND 10000000));

-- Like every other SECURITY DEFINER function: no EXECUTE for PUBLIC, only for
-- the app role the policies run as.
REVOKE ALL ON FUNCTION app_organizer() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_organizer() TO ts_app;

-- +goose Down
GRANT EXECUTE ON FUNCTION app_organizer() TO PUBLIC;
DELETE FROM promo_codes WHERE value = 0;
ALTER TABLE promo_codes DROP CONSTRAINT promo_codes_value_check;
ALTER TABLE promo_codes ADD CONSTRAINT promo_codes_check
  CHECK ((kind = 'percent' AND value BETWEEN 1 AND 10000) OR (kind = 'amount' AND value BETWEEN 1 AND 10000000));

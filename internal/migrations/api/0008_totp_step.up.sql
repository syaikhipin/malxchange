-- TOTP consumption (audit issue #103): remember the newest step whose
-- code verified, so a replayed code inside the +/-1-step window is
-- rejected instead of authorizing a second action.
ALTER TABLE authenticator_bindings
    ADD COLUMN last_used_step BIGINT NOT NULL DEFAULT 0;

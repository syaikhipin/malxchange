-- TOTP consumption (audit issue #103), employee side.
ALTER TABLE employees
    ADD COLUMN last_used_step BIGINT NOT NULL DEFAULT 0;

ALTER TABLE accounts ADD COLUMN r2_from_api_token INTEGER NOT NULL DEFAULT 0 CHECK (r2_from_api_token IN (0, 1));

CREATE TABLE runner_controller_epochs (
    provider_id VARCHAR(128) PRIMARY KEY CHECK (provider_id <> ''),
    epoch BIGINT NOT NULL CHECK (epoch > 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

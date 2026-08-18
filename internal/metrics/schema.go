package metrics

// SchemaVersion is bumped whenever migrations are appended. Migrations are
// forward-only and each must be safe to run against a database already at a
// later version (they are skipped), so an older binary never rewrites a newer
// schema.
const SchemaVersion = 1

// migrations are applied in order; index+1 is the version each produces.
var migrations = []string{
	`
CREATE TABLE IF NOT EXISTS requests (
  id             INTEGER PRIMARY KEY,
  started_at     INTEGER NOT NULL,
  duration_ms    INTEGER,
  ttfb_ms        INTEGER,
  account_id     TEXT NOT NULL,
  provider       TEXT NOT NULL,
  model          TEXT,
  upstream_model TEXT,
  session_id     TEXT,
  endpoint       TEXT,
  status         INTEGER,
  outcome        TEXT,
  stream         INTEGER,
  attempts       INTEGER,
  rotated        INTEGER,
  wait_ms        INTEGER,
  input_tokens        INTEGER,
  output_tokens       INTEGER,
  cache_read_tokens   INTEGER,
  cache_write_tokens  INTEGER,
  cost_micros    INTEGER
);
CREATE INDEX IF NOT EXISTS requests_started       ON requests(started_at);
CREATE INDEX IF NOT EXISTS requests_acct_started  ON requests(account_id, started_at);
CREATE INDEX IF NOT EXISTS requests_model_started ON requests(model, started_at);

CREATE TABLE IF NOT EXISTS quota_samples (
  at          INTEGER NOT NULL,
  account_id  TEXT    NOT NULL,
  bucket      TEXT    NOT NULL,
  utilization REAL,
  resets_at   INTEGER,
  PRIMARY KEY (at, account_id, bucket)
);

CREATE TABLE IF NOT EXISTS usage_buckets (
  bucket_start INTEGER NOT NULL,
  granularity  TEXT    NOT NULL,
  account_id   TEXT    NOT NULL,
  model        TEXT    NOT NULL,
  requests     INTEGER NOT NULL,
  input_tokens       INTEGER NOT NULL,
  output_tokens      INTEGER NOT NULL,
  cache_read_tokens  INTEGER NOT NULL,
  cache_write_tokens INTEGER NOT NULL,
  cost_micros        INTEGER,
  PRIMARY KEY (bucket_start, granularity, account_id, model)
);
`,
}

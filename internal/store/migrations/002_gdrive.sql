-- ---------------------------------------------------------------------------
-- Google Drive destinations
--
-- Drive has no buckets, regions or signed requests, so it brings its own
-- columns rather than borrowing the S3 ones. Existing rows keep provider 's3'
-- or 'oci' and leave every column here NULL.
-- ---------------------------------------------------------------------------

ALTER TABLE destinations ADD COLUMN drive_folder_id TEXT;
ALTER TABLE destinations ADD COLUMN drive_id        TEXT;   -- shared drive, if any
ALTER TABLE destinations ADD COLUMN drive_subject   TEXT;   -- impersonated user
ALTER TABLE destinations ADD COLUMN oauth_client_id TEXT;

ALTER TABLE destinations ADD COLUMN service_account_json_enc BLOB;  -- AES-256-GCM
ALTER TABLE destinations ADD COLUMN oauth_client_secret_enc  BLOB;  -- AES-256-GCM
ALTER TABLE destinations ADD COLUMN oauth_refresh_token_enc  BLOB;  -- AES-256-GCM

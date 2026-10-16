# Feather

[![ci](https://github.com/partha7278/feather/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/partha7278/feather/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/partha7278/feather)](https://goreportcard.com/report/github.com/partha7278/feather)
[![License](https://img.shields.io/github/license/partha7278/feather)](LICENSE)

MongoDB and PostgreSQL retention for a VPS, with a control panel. On a
schedule, feather selects old documents or rows by a condition you define,
writes them out compressed to several cold-storage destinations at once,
verifies every copy, and — if that config says so — deletes them from the
source. Every archive is catalogued, so "where did the January orders go?" is a
page you can open rather than a guess.

- **One database.** Accounts, retention configs, the administrator login, and
  the catalogue of every file ever written all live in a single SQLite file.
  `feather backup` writes a consistent snapshot of it while Feather keeps
  running.
- **A control panel.** Add MongoDB and PostgreSQL accounts and S3, Oracle or Google Drive
  destinations, write retention rules, watch runs, and browse every archive from
  the browser. Changes take effect without a restart.
- **Many accounts.** Any number of MongoDB clusters, PostgreSQL databases, and
  S3, Oracle Cloud and Google Drive accounts. Each config names the database it
  reads and the destinations it writes; every destination gets a full copy.
- **Deletion is a per-config switch.** Off, Feather is a backup tool. On, it is
  a retention tool: the documents go once every copy is stored and verified.
- **Encrypted credentials.** Connection strings and cloud secrets are sealed
  with AES-256-GCM under a master key held outside the database.
- **Portable.** Every account, destination and retention config exports as one
  file — credentials re-sealed under a passphrase you choose — and imports into
  another feather.
- **Your file names.** The name of every archive file is a template you write.

## Install

```bash
make build              # -> bin/feather
sudo make install       # -> /usr/local/bin/feather
```

Or with Docker: `docker build -t feather .`

## Start

```bash
feather serve
```

That is the whole setup. On first start Feather creates its database and master
key under `/var/lib/feather` (or `./data` if that is not writable), then prints
the control panel URL — `http://127.0.0.1:8477` by default.

Open it and the first-run journey asks for an **administrator email and
password**. Nothing else exists until that account does; nothing else is
reachable until you are signed in.

From there:

1. **MongoDB or PostgreSQL accounts** — add the database to archive from. Test
   the connection.
2. **Destinations** — add an S3 bucket, an Oracle bucket, a Google Drive folder,
   or several. Test each.
3. **Retention configs** — what to archive, where the copies go, when, and
   whether the originals are deleted afterwards.

Behind a reverse proxy, add `--secure-cookies` so session cookies are marked
`Secure`.

### Running as a service

```bash
sudo cp deploy/feather.service /etc/systemd/system/
sudo systemctl enable --now feather
journalctl -u feather -f
```

## Configuration that stays in the environment

Only process-level settings — see `.env.example`. There are no credentials in
it: those live in the database.

| Variable | Default | Meaning |
|---|---|---|
| `FEATHER_DATA_DIR` | `/var/lib/feather` | Database, master key and spool files |
| `FEATHER_HTTP_ADDR` | `127.0.0.1:8477` | Where the control panel listens |
| `FEATHER_MASTER_KEY` | — | Encryption key as 64 hex characters; generated into a file if unset |
| `FEATHER_BASE_URL` | — | The externally visible URL behind a proxy; also builds the Google Drive redirect URI |
| `FEATHER_MAX_CONCURRENT_JOBS` | `2` | How many jobs may archive at once |
| `LOG_LEVEL`, `LOG_JSON` | `info`, `true` | Logging |

### The master key

Credentials are encrypted before they are written. The key comes from
`FEATHER_MASTER_KEY` if set, otherwise from `FEATHER_DATA_DIR/master.key`,
which Feather generates with mode 0600 on first start.

```bash
feather keygen          # prints a key suitable for FEATHER_MASTER_KEY
```

**Keep the key with your backups.** A copy of the database without it cannot be
read, which is the point — and also means a restore needs both.

**What is encrypted.** MongoDB and PostgreSQL connection strings and every cloud secret
(S3 secret keys and session tokens, Google service-account JSON, OAuth client
secrets and refresh tokens) are sealed with AES-256-GCM
under the master key, each with its own random nonce, before they reach the
database. Everything else — retention rules, the archive catalogue, the run
history — is stored in the clear. Administrator passwords are bcrypt hashes and
do not involve the key.

**What the logs say about it.** On every start Feather logs where the key came
from, so a swapped or regenerated key is visible before anything fails:

```
level=INFO msg="feather ready" database=/var/lib/feather/feather.db master_key=/var/lib/feather/master.key ...
```

`master_key` is `FEATHER_MASTER_KEY` when the key came from the environment, the
file path when it was read from disk, and `<path> (newly generated)` on the
first start — seeing that last one on a server that already had data means the
old key is gone. A key file readable by group or other stops startup with the
`chmod 600` to fix it, and `/readyz` returns 503 if the key in use cannot open
what is already sealed.

**Downloading the key.** **Settings → Storage → Download master key** saves it as
`feather-master.key` after asking for your password again. It serves whichever
key the running process uses — the environment value or the file. Every download
is written to the audit log as `settings.master_key_downloaded`, with who, from
which IP and the key's source, and logged at warning level as
`master key downloaded`; a wrong password is logged as `master key download
refused`. Store the file the way you store the backups, not next to them in the
same unencrypted bucket.

## A retention config

Everything is edited in the control panel; this is what a config holds.

**Source** — the MongoDB account, the database and collection, and the filter.
The filter is ordinary MongoDB Extended JSON, so `$lt`, `$in`, `$and`, `$oid`
and `$date` all work as written. The one addition is relative-time tokens:

| Token | Becomes |
|---|---|
| `{{now}}` | the current instant |
| `{{now-90d}}` | 90 days ago |
| `{{now-6h}}` | 6 hours ago |
| `{{now+1d}}` | tomorrow |

They resolve on every run, which is what makes "older than 90 days" keep
meaning that. A token that is not recognised is an error, not a literal.

**A PostgreSQL source** — the account, the schema (default `public`) and the
table, and a filter written as the condition of a `WHERE` clause:

```sql
created_at < {{now-90d}} AND status IN ('closed', 'void')
```

A bare token becomes a typed literal, `TIMESTAMPTZ '2026-02-13T02:30:00.000Z'`;
inside quotes, `'{{now-90d}}'`, it becomes just the timestamp text. Subqueries
and functions work — it is real SQL, checked by PostgreSQL.

The same condition guards every delete (`... WHERE id = ANY($1) AND (<filter>)`),
so Feather refuses anything that could reach outside its parentheses: a `;`,
`--` or `/* */` comments, `$` parameters or dollar quoting, `E'...'` strings,
and unbalanced parentheses or quotes. It is checked when the config is saved
and again immediately before each run touches the table.

What a table needs, and why:

- **A single-column primary key.** Rows are read in key order, a page at a time
  (`WHERE key > last ORDER BY key LIMIT n`), not through a cursor held open for
  the whole run — a transaction that lasts as long as an archive pins
  PostgreSQL's xmin horizon, and autovacuum stops cleaning every table on the
  server until it ends. The key is also what a run is fenced at and what rows
  are deleted by. Tables with a composite key, or none, are refused with that
  reason.
- **PostgreSQL 12 or newer**, and an account with `SELECT` on the table —
  plus `DELETE` if the config deletes.

Each row is archived as `row_to_json`, PostgreSQL's own rendering of it, which
`json_populate_record` reads back to the same values: timestamps keep their
microseconds, numerics keep every digit, and arrays, `bytea`, `uuid`, `jsonb`
and composite values survive the round trip. The one thing not kept byte for
byte is insignificant whitespace inside a `json` (not `jsonb`) column, whose
newlines become spaces so each row stays on one line. A projection is not
offered: a partial row cannot be restored. The timestamp column is any column
that casts to `timestamptz`, read as UTC.

**Destinations** — one or more, of any kind. Each receives a complete copy, and
no document is deleted until every copy is verified. Mixing providers is the
point: an S3 bucket and a Google Drive folder on the same config are two copies
that fail for different reasons.

**File names** — the template that names each archive file. Slashes become
folders in the bucket, and real folders in Google Drive:

```
archive/{db}/{collection}/{yyyy}-{MM}/{config}-{runId}-{seq}{ext}
```

| Variable | Becomes |
|---|---|
| `{config}` | the config's name |
| `{db}`, `{collection}` | source database and collection |
| `{runId}` | unique ID of the run |
| `{seq}` | batch number within the run, zero-padded to 5 |
| `{yyyy}`, `{MM}`, `{dd}`, `{HH}`, `{mm}` | run start, UTC |
| `{ts}` | full timestamp, e.g. `20260514T023000Z` |
| `{ext}` | extension implied by the codec, e.g. `.ndjson.gz` |

A template **must contain `{runId}`**, and an unknown variable is rejected when
you save rather than written into an object key literally.

`{runId}` is required rather than merely recommended, and `{seq}` is not a
substitute for it. `{seq}` distinguishes the batches within one run, but it
repeats from `00001` on the next run — so a template built from `{seq}` alone
renders the same keys every time, and the second run writes over the first
run's objects. When deletion is on, those documents are already gone from
MongoDB, and the archive that held the only copy of them has just been
replaced. Including `{runId}` makes every run's keys unique, which is also why
a retry after a failure leaves the previous attempt's objects intact.

**After archiving** — the deletion switch. Off, documents are copied and left
alone. On, once every destination has the file and each copy has been read back
and checked, the batch is deleted from MongoDB. It is togglable on its own from
the config list, without editing anything else.

**Schedule** — either an interval or a cron expression:

```
every 2d at 02:30  (Asia/Kolkata)
cron 30 2 * * *    (Asia/Kolkata)
```

Prefer the interval. Cron cannot express "every 2 days": `*/2` is
day-of-month based and produces a one-day gap at each month boundary. An
interval is anchored on the last completed run, so restarts do not reset the
cycle, and a window missed while the host was down is caught up once rather
than skipped. `at` pins the time of day and requires an interval of 24h or more.

**Limits** — `maxDocsPerRun` caps a whole run, which is worth setting the first
time you point this at a large collection; `maxRuntime` bounds a run;
disabling a config parks it without deleting it.

## Google Drive

Drive has no buckets, no regions and no signed requests, so its destination form
asks for different things: a folder, and a Google account to write as. Archives
are filed into that folder, with each slash in the file name template becoming a
real Drive folder underneath it.

**A folder.** Paste its ID or the browser URL of the folder you are looking at —
both work. Left blank, archives go to the account's own My Drive. If the folder
lives in a shared drive, give the shared drive's ID too.

**An OAuth client** is the usual way in, and the only one that works with a
personal Google account, because it writes as a person and uses that person's
storage.

1. In the Google Cloud console, enable the **Google Drive API** for a project.
2. Create an OAuth client of type **Web application**, and register Feather's
   callback as an authorised redirect URI. The destination form prints the exact
   URI to paste; it is your panel's address plus
   `/destinations/gdrive/callback`. Set `FEATHER_BASE_URL` when Feather is
   behind a reverse proxy, so the URI it prints is the one Google will be
   handed.
3. Paste the client ID and secret into the destination and save it. The account
   is stored, but nothing can write to it yet.
4. Press **Connect Google Drive**. Google asks for offline access to Drive and
   sends you back; Feather keeps the refresh token, sealed like every other
   credential, and renews its own access from then on.

Disconnecting forgets that token. The account and its folder stay, so
reconnecting is one press, and archives already written are untouched.

**A service account** needs no browser, which suits an unattended VPS — but a
service account owns no Drive storage of its own. It can only write into a
**shared drive** that somebody has shared with the key's `client_email`, giving
it edit access; pointed at an ordinary folder it will upload nothing and report
a quota error. Paste the whole downloaded JSON key file into the destination.
With Workspace domain-wide delegation, the **Impersonate** field names the user
to act as.

Verification works as it does everywhere else: after the upload, Feather re-reads
the file and compares its size and SHA-256 — including the checksum Drive
computes itself, which is an end-to-end check of what actually landed.

## The command line

The panel is the normal way to work, but everything important has a command:

```bash
feather serve                    # control panel + scheduler (the usual way)
feather daemon                   # scheduler only, no web UI
feather run <config> [--dry-run]
feather validate                 # check accounts, configs, connectivity; print matched counts
feather verify <run|archive|config>      # re-read stored archives, confirm they are intact
feather restore <run|archive|config> [--into db.coll] [--dry-run]
feather backup <file>            # consistent snapshot of Feather's own database
feather list                     # configs, schedules, last run status
feather export <file>            # every account, config and setting, as one file
feather import-bundle <file>     # read one back in
feather import                   # bring an older .env + configs/*.json installation into the database
feather create-admin <email>
feather reset-password <email>   # for when you are locked out
feather keygen
```

Always `validate` and then `--dry-run` before the first real run of a config
that deletes. A dry run reads and compresses everything, reports exactly what it
would upload and delete, and touches neither the destinations nor MongoDB.

## Backing Feather up

Feather's own state — the catalogue, the administrator logins, the sealed
credentials — is one SQLite file. Copying that file while Feather is running is
**not** a backup: it is written ahead of itself in WAL mode, so a plain `cp`
captures a torn state missing the most recent transactions.

```bash
feather backup /var/backups/feather-$(date -u +%Y%m%dT%H%M%SZ).db
```

That takes a consistent snapshot through a transaction, so Feather can keep
archiving while it runs. An existing file is never overwritten.

**The snapshot needs the master key to be read.** Keep a copy of the key
wherever you keep the backups; a restore needs both. Restoring is putting the
file back at `FEATHER_DATA_DIR/feather.db` with the key beside it and starting
feather.

Note the division of labour: a **backup** is the whole state and is what you
restore from, while an **export bundle** (below) is a portable copy of the
configuration and deliberately carries neither the catalogue nor the logins.

## Moving an installation

Every MongoDB account, every destination, every retention config and the
instance settings can be written out as one file and read into another Feather —
from **Settings → Export & import** in the panel, or from the command line.

```bash
feather export ./feather-bundle.json          # asks for a passphrase
feather import-bundle ./feather-bundle.json   # asks for the same one
```

The credentials are the reason this is not a plain dump. They are why feather
has a master key at all, and a bundle by definition leaves the machine holding
that key — so on the way out every sealed value is re-sealed under a passphrase
you type, with scrypt between the passphrase and the AES key. The master key
itself never travels. **Lose the passphrase and the bundle is unopenable**; keep
it wherever you keep the master key.

Add `--no-secrets` (the *export without credentials* tick in the panel) for the
other kind of export: accounts, destinations and retention rules with nothing
secret in them. That file needs no passphrase and is the one to keep in version
control or hand to somebody else. Importing it can add retention configs and
refresh accounts that are already there, but it cannot create an account it has
no credentials for — those are listed as skipped, to be added by hand.

An import only adds what is missing, so running it twice changes nothing the
second time. `--overwrite` (*replace entries that already exist* in the panel)
makes it replace instead — which is what a restore onto a rebuilt server wants,
and what a merge into a live instance usually does not.

Imported configs are live at once, deletion switches included. Read them before
the next scheduled run.

### Migrating from the file-based version

Earlier versions kept credentials in `.env` and one JSON file per job. To bring
those across:

```bash
feather import --from-env .env --from-configs ./configs
```

Accounts and configs already in the database are left alone, so it is safe to
repeat. Afterwards those files are no longer read, and the credentials in them
can be removed.

## What a run does, in order

1. Take the config's lease. A second daemon, or a manual run during a scheduled
   one, skips instead of double-archiving.
2. Resolve the filter's time tokens and open a run row in the catalogue.
3. Stream matching documents in `_id` order, writing canonical Extended JSON
   lines through a compressor into a local spool file, until a batch limit is hit.
4. Upload that file to every destination in parallel.
5. Re-read each stored object and check its size and checksum.
6. If the config deletes: remove the batch's documents from MongoDB — chunked,
   and re-applying the original filter alongside the id list.
7. Discard the spool file and move to the next batch.

Step 6 happens only if step 5 succeeded everywhere. If any destination fails,
the run stops, it is recorded as failed or partial, and **no documents are
deleted** — the next run picks up the same documents, since they are still there
and still match.

The filter is re-applied at delete time on purpose. If a document was modified
between being read and being deleted so that it no longer matches the retention
condition, it stays. The archive then holds a copy of a row that still exists,
which is harmless; deleting a row that no longer qualified would not be.

## Where the data went

**Backups** in the panel lists every archive file: its name, when it was
written, the date range of the documents inside it, how many there are, its
compressed and raw size, whether the source documents were deleted, and the
full location of every copy. Filter by config, search by file name or object
key, or narrow to archives whose source data was deleted.

The same thing is a SQL query away, since it is one SQLite file:

```sql
-- every copy of everything archived from one collection in January
select a.min_ts, a.max_ts, a.doc_count, a.file_name,
       d.destination_alias, d.location_url, d.status
from feather_artifacts a
join feather_artifact_destinations d on d.artifact_id = a.id
where a.collection_name = 'orders'
  and a.min_ts >= '2026-01-01' and a.max_ts < '2026-02-01'
order by a.min_ts, d.destination_alias;
```

The tables: `feather_runs` (one row per execution), `feather_artifacts` (one per
file), `feather_artifact_destinations` (one per copy), alongside `mongo_sources`,
`destinations`, `job_configs` and `admin_users`.

## Restoring

```bash
feather restore <run id> --dry-run     # read every archive, check it, write nothing
feather restore <run id>               # put the documents back where they came from
feather restore <run id> --into shop.orders_recovered
```

PostgreSQL archives go back into the table they came from, through the account
their config reads, or into another with `--into schema.table` — which must
already exist with compatible columns, for example one made with
`CREATE TABLE sales.orders_recovered (LIKE sales.orders INCLUDING ALL)`. Rows
whose key is already present are counted and left alone. Generated columns are
recomputed rather than written, and an identity key goes back as it was
(`OVERRIDING SYSTEM VALUE`). The control panel's **Imports** write into MongoDB
only; a PostgreSQL archive is restored from the command line.

A restore reads each archive back, hashes it as it goes, and refuses to write
anything if the digest does not match what the catalogue recorded — so a copy
that has rotted, been overwritten, or been expired by a lifecycle rule fails
loudly instead of returning wrong data. Documents that are already present are
counted, not treated as errors, so restoring twice converges rather than failing
halfway.

Take the same target one step further with `--into` when you want the documents
somewhere they cannot collide with live data before you look at them.

### Checking archives without restoring

```bash
feather verify <run id>          # or an archive id, or a config name
```

`verify` re-reads every stored copy of every archive it names and confirms each
one still hashes to what the catalogue says. Verification happens once at write
time; nothing after that ever looked at the object again, so bit-rot, an
accidental bucket delete and a lifecycle rule expiring an archive were all
invisible until somebody needed the data. Run it on a schedule against the
configs that delete, and it finds those while another copy still exists to
repair from.

It exits non-zero when an archive has no readable copy left — which is the
condition worth alerting on, rather than any single copy failing.

### By hand

Archives are gzipped NDJSON in canonical Extended JSON, which `mongoimport`
reads directly. ObjectId, Date, Decimal128 and int32-vs-int64 all survive the
round trip.

```bash
aws s3 cp s3://unifers-archive/archive/shop/orders/2026-05/orders-archive-20260514T023000Z-00001.ndjson.gz .
gunzip -c orders-archive-20260514T023000Z-00001.ndjson.gz \
  | mongoimport --db shop --collection orders --type=json
```

For zstd archives, use `zstd -dc` in place of `gunzip -c`.

A PostgreSQL archive is one `row_to_json` object per line. `psql` can load it
through a staging table — the `csv` options stop `COPY` from treating
backslashes and quotes in the JSON as its own escapes:

```bash
gunzip -c orders-pg-….ndjson.gz | psql "$DSN" -c "
  CREATE TEMP TABLE feather_load (row json);
  COPY feather_load FROM STDIN WITH (FORMAT csv, QUOTE e'\x01', DELIMITER e'\x02');
  INSERT INTO sales.orders OVERRIDING SYSTEM VALUE
    SELECT (json_populate_record(NULL::sales.orders, row)).* FROM feather_load
    ON CONFLICT DO NOTHING;"
```

Leave generated columns out of that insert by naming the columns, or use
`feather restore`, which does.

A copy in Google Drive is a file like any other: open the folder in a browser
and download it, or fetch it by the Drive file ID the catalogue recorded against
that copy. What comes down is byte-for-byte what went up, so the same two
commands finish the restore.

## Importing archives

The control panel can do what `feather restore` does without a shell. Open
**Imports** in the sidebar, or start from the file you want:

- **Backups → Import into MongoDB** on any archive's row imports that one file.
- **Import this run** on a finished run's page imports every archive it wrote,
  oldest batch first.
- **Imports → New import** uploads a file from your computer instead: NDJSON in
  canonical or relaxed Extended JSON (what `mongoexport` writes), as `.ndjson`,
  `.jsonl` or `.json`, plain or compressed as `.gz` or `.zst`, up to 256 MiB.

The defaults are the safe choice. For a catalogued archive the form fills in the
MongoDB account its config reads through, the database it came from, and a new
collection, `<collection>_restored_<YYYYMMDD>`, so accepting them never writes
into live data. The account needs write access (`readWrite`) to the database
you choose.

Every import is one of two modes:

- **Add missing documents only** (the default). A document whose `_id` is
  already there is left alone and counted as already present, so running the
  same import twice changes nothing the second time.
- **Replace documents with the same _id.** The archived version overwrites what
  is there now. Feather asks for your password again before a real replace, and
  records it in the audit log as destructive.

Nothing is ever dropped or deleted. **Dry run** reads every file, checks each
archive's SHA-256 and document count against the catalogue, and writes nothing.
A finished dry run of a catalogued archive offers **Import for real** with the
same settings. If the target collection is one an enabled config archives and
deletes from, the form says so and asks you to confirm: documents that still
match the config's filter are deleted again on its next run.

One import runs at a time. Its page shows progress while it runs and the final
counts (read, inserted, already present, replaced) when it finishes, and the
start and the outcome both go to the audit log. There is no cancel button: an
import interrupted by a restart is marked failed, and running it again in
"add missing" mode picks up where it stopped without duplicating anything.

## Verifying locally

A rig with MongoDB, PostgreSQL (on port 5433) and MinIO standing in for the
object stores:

```bash
make rig-up
make rig-seed                 # 200k orders over 6 months
make rig-serve                # imports the rig's accounts, then opens the panel
```

Or drive it from the command line:

```bash
./bin/feather validate --env testdata/.env.rig
./bin/feather run test-archive --env testdata/.env.rig --dry-run
./bin/feather run test-archive --env testdata/.env.rig
make rig-down
```

MinIO exercises the same non-AWS code path Oracle needs — custom endpoint,
path-style addressing, relaxed request checksums.

## Watching it

Three endpoints, none of which need a session:

| Endpoint | Answers |
|---|---|
| `/livez` | Is the process up? Touches nothing, so a busy database cannot get Feather restarted. |
| `/readyz` | Can it work? Checks the database answers and the master key opens what is sealed with it. 503 when not. |
| `/metrics` | Prometheus text format. |

`/healthz` still exists and means `/livez`.

The metric worth an alert is **`feather_config_last_success_age_seconds`**. A
config that has been failing every night looks identical to a healthy one
everywhere except the runs page, and only if somebody opens it:

```
- alert: FeatherRetentionStalled
  expr: feather_config_last_success_age_seconds > 172800
  for: 1h
  annotations:
    summary: "{{ $labels.config }} has not archived successfully in two days"
```

A config that has never succeeded is absent from that gauge rather than reported
as age zero, which would read as "just succeeded". Alongside it are
`feather_config_consecutive_failures`, `feather_config_parked` (the config no
longer parses and is not running at all), `feather_config_deletes_source`, and
the usual totals for documents archived, documents deleted and bytes stored.

**Notifications.** Set a webhook under Settings and Feather POSTs one JSON object
when a run fails. Credentials are stripped from the message on the way out, dry
runs never notify, and a webhook that is down or slow is logged and ignored —
the thing that reports outages must not be able to cause them. Tick *notify on
success* if you would rather have a heartbeat than silence.

## Notes

- Oracle Cloud is reached through its S3-compatible endpoint. The credentials
  are an OCI **Customer Secret Key** (Identity → Users → your user → Customer
  Secret Keys), not an API signing key. Choosing the Oracle provider also
  relaxes the request checksums recent AWS SDKs send by default, which Oracle's
  endpoint rejects, and forces path-style addressing.
- Object keys carry the run ID, so a retry writes new objects rather than
  overwriting a previous attempt. A failed run can leave objects behind in the
  destinations that did succeed; they are recorded in the catalogue against a
  failed run and are safe to delete.
- Multipart ETags are not object MD5s, so verification uses the size plus the
  SHA-256 stored in the object's metadata. Google Drive has no ETag worth
  keeping either: the catalogue's `etag` column holds the Drive file ID instead,
  which is what you need to find a copy again, and `location_url` reads
  `gdrive://<folder>/<path>`.
- Google Drive is asked for full Drive access rather than the narrower
  `drive.file` scope, because that scope cannot see a folder Feather did not
  create — including the one you point it at.
- An empty filter combined with deletion is rejected — that combination would
  archive and then empty the whole collection.
- Runs that a crashed process left open are closed at the next start, stale
  leases are cleared, and orphaned spool files are removed.
- The control panel binds to localhost by default. Put it behind a reverse
  proxy with TLS before exposing it, and pass `--secure-cookies` when you do.

# feather

MongoDB retention for a VPS, with a control panel. On a schedule, feather
selects old documents by a condition you define, writes them out compressed to
several cold-storage destinations at once, verifies every copy, and — if that
config says so — deletes them from MongoDB. Every archive is catalogued, so
"where did the January orders go?" is a page you can open rather than a guess.

- **One database.** Accounts, retention configs, the administrator login, and
  the catalogue of every file ever written all live in a single SQLite file.
  Backing feather up is a file copy.
- **A control panel.** Add MongoDB accounts and S3, Oracle or Google Drive
  destinations, write retention rules, watch runs, and browse every archive from
  the browser. Changes take effect without a restart.
- **Many accounts.** Any number of MongoDB clusters and any number of S3, Oracle
  Cloud and Google Drive accounts. Each config names the cluster it reads and
  the destinations it writes; every destination gets a full copy.
- **Deletion is a per-config switch.** Off, feather is a backup tool. On, it is
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

That is the whole setup. On first start feather creates its database and master
key under `/var/lib/feather` (or `./data` if that is not writable), then prints
the control panel URL — `http://127.0.0.1:8477` by default.

Open it and the first-run journey asks for an **administrator email and
password**. Nothing else exists until that account does; nothing else is
reachable until you are signed in.

From there:

1. **MongoDB accounts** — add the cluster to archive from. Test the connection.
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
which feather generates with mode 0600 on first start.

```bash
feather keygen          # prints a key suitable for FEATHER_MASTER_KEY
```

**Keep the key with your backups.** A copy of the database without it cannot be
read, which is the point — and also means a restore needs both.

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

**Destinations** — one or more, of any kind. Each receives a complete copy, and
no document is deleted until every copy is verified. Mixing providers is the
point: an S3 bucket and a Google Drive folder on the same config are two copies
that fail for different reasons.

**File names** — the template that names each archive file. Slashes become
folders in the bucket, and real folders in Google Drive:

```
archive/{db}/{collection}/{yyyy}-{MM}/{config}-{ts}-{seq}{ext}
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

A template must contain `{runId}` or `{seq}` so two batches cannot overwrite
each other, and an unknown variable is rejected when you save rather than
written into an object key literally.

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
2. Create an OAuth client of type **Web application**, and register feather's
   callback as an authorised redirect URI. The destination form prints the exact
   URI to paste; it is your panel's address plus
   `/destinations/gdrive/callback`. Set `FEATHER_BASE_URL` when feather is
   behind a reverse proxy, so the URI it prints is the one Google will be
   handed.
3. Paste the client ID and secret into the destination and save it. The account
   is stored, but nothing can write to it yet.
4. Press **Connect Google Drive**. Google asks for offline access to Drive and
   sends you back; feather keeps the refresh token, sealed like every other
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

Verification works as it does everywhere else: after the upload, feather re-reads
the file and compares its size and SHA-256 — including the checksum Drive
computes itself, which is an end-to-end check of what actually landed.

## The command line

The panel is the normal way to work, but everything important has a command:

```bash
feather serve                    # control panel + scheduler (the usual way)
feather daemon                   # scheduler only, no web UI
feather run <config> [--dry-run]
feather validate                 # check accounts, configs, connectivity; print matched counts
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

## Moving an installation

Every MongoDB account, every destination, every retention config and the
instance settings can be written out as one file and read into another feather —
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

Archives are gzipped NDJSON in canonical Extended JSON, which `mongoimport`
reads directly. ObjectId, Date, Decimal128 and int32-vs-int64 all survive the
round trip.

```bash
aws s3 cp s3://unifers-archive/archive/shop/orders/2026-05/orders-archive-20260514T023000Z-00001.ndjson.gz .
gunzip -c orders-archive-20260514T023000Z-00001.ndjson.gz \
  | mongoimport --db shop --collection orders --type=json
```

For zstd archives, use `zstd -dc` in place of `gunzip -c`.

A copy in Google Drive is a file like any other: open the folder in a browser
and download it, or fetch it by the Drive file ID the catalogue recorded against
that copy. What comes down is byte-for-byte what went up, so the same two
commands finish the restore.

## Verifying locally

A rig with MongoDB and MinIO standing in for the object stores:

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
  `drive.file` scope, because that scope cannot see a folder feather did not
  create — including the one you point it at.
- An empty filter combined with deletion is rejected — that combination would
  archive and then empty the whole collection.
- Runs that a crashed process left open are closed at the next start, stale
  leases are cleared, and orphaned spool files are removed.
- The control panel binds to localhost by default. Put it behind a reverse
  proxy with TLS before exposing it, and pass `--secure-cookies` when you do.

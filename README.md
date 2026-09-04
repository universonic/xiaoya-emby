# Xiaoya Utility for Emby

Utility to maintain metadata files in xiaoya media library for Emby.

It is an alternative to the `xiaoya-emd` utility, with boosted performance and emhanced feature.

### Build

The default targets are Linux and macOS for `amd64` and `arm64`:

```bash
make
```

Build for specific platform (linux-amd64):

```bash
make linux-amd64
```

Build for specific platform (linux-arm64):

```bash
make linux-arm64
```

Go 1.25.1 is required.
SQLite requires CGO, so every target builds with `CGO_ENABLED=1`. Native builds use the system C compiler; cross-builds also require a matching C cross-compiler configured through `CC`. An unsupported cross-build fails instead of producing a binary with a nonfunctional SQLite stub.

### Usage (Command-Line)

```txt
Utility to maintain metadata files in xiaoya media library for Emby

Usage:
  xiaoya-emby [flags]

Flags:
      --alist-path-skip-verify strings            Specify the Alist path to skip verify files. For example: "/🏷️我的115分享".
      --alist-path-skip-verify-from-file string   A file contains a list of Alist path to skip verify.
  -r, --alist-strm-root-path string               Root path of strm files in xiaoya Alist. (default "/d")
  -u, --alist-url string                          Endpoint of xiaoya Alist. Change this value will result to url overide in strm file. (default "http://xiaoya.host:5678")
      --cleanup                                   Cleanup downloaded metadata when file no longer exists on remote server.
      --control-token string                      Bearer token protecting the control API. Env: CONTROL_TOKEN (used when the flag is unset). Without a token the web interface is read-only; serve behind TLS.
      --cron-expr string                          Cron expression as scheduled task. Must run as daemon. (default "0 0 * * *")
      --daemon                                    Run as daemon in foreground. (default true)
  -D, --download-dir string                       Download cache directory for metadata. (default "/download")
      --download-workers int                      Number of concurrent download workers. 0 means auto (min(CPU, 8)).
      --force-crawl                               Force HTML crawling mode instead of manifest-based metadata sync.
  -h, --help                                      Print this message.
      --listen-addr string                        Address for the status page (progress, logs and, when permitted, manual controls). Set to "" to disable. (default "127.0.0.1:9527")
  -l, --log-level string                          Minimum log level (debug, info, warn, error). Env: LOG_LEVEL. (default "info")
  -d, --media-dir string                          Media library directory maintained for Emby. (default "/media")
  -m, --mirror-url strings                        Specify the mirror URL to sync metadata from.
      --mode int                                  Run mode (4: update download cache, 2: reserved, 1: sync media library) (default 7)
  -p, --purge                                     Whether to purge useless file or directory when media is no longer available. (default true)
      --strm-path-skip-verify strings             Specify the metadata path to skip verify strm files. For example: "/115".
      --strm-path-skip-verify-from-file string    A file contains a list of strm path to skip verify.
  -v, --version                                   Print software version.
```

### Quickstart

The container runs as UID/GID `568:568`. Both the download cache directory and media library directory must already exist and be writable by that account. On Linux:

```bash
export MY_DOWNLOAD_FOLDER=/path/to/xiaoya-download
export MY_MEDIA_FOLDER=/path/to/emby-media
sudo mkdir -p "$MY_DOWNLOAD_FOLDER" "$MY_MEDIA_FOLDER"
sudo chown 568:568 "$MY_DOWNLOAD_FOLDER" "$MY_MEDIA_FOLDER"

docker run --rm --user 568:568 --entrypoint /bin/bash \
  -v "$MY_DOWNLOAD_FOLDER:/download" \
  -v "$MY_MEDIA_FOLDER:/media" \
  universonic/xiaoya-emby:latest \
  -c 'test -w /download && test -w /media'
```

On SELinux hosts, add an appropriate bind-mount label such as `:z`. Then start the daemon:

```bash
docker run -d --name xiaoya-emby --restart unless-stopped \
  -v "$MY_DOWNLOAD_FOLDER:/download" \
  -v "$MY_MEDIA_FOLDER:/media" \
  -v /etc/localtime:/etc/localtime:ro \
  -p 127.0.0.1:9527:9527 \
  universonic/xiaoya-emby:latest \
  --listen-addr 0.0.0.0:9527
```

The `/etc/localtime` bind makes Cron use the Linux host timezone; omit or replace it when Docker Desktop does not expose that path. The host port is deliberately bound to `127.0.0.1`. Without `CONTROL_TOKEN` or `--control-token`, the page is read-only. `purge=true` is enabled by default and removes media-library entries only after Alist explicitly confirms that their targets are unavailable; use `--purge=false` to disable it.

### Metadata Sync Modes

By default the program syncs metadata via the pre-built manifest (`/.scan.list.gz`) published by the mirrors: all mirrors are probed concurrently, only the ones serving exactly the newest manifest generation are used, and every incremental trigger downloads and parses that manifest before computing the file-level diff. This guarantees that a database row cannot hide a missing cache file: listed files that are absent on disk are downloaded again even when the manifest generation itself has not changed. When the manifest is unavailable on every mirror, the program automatically falls back to the legacy HTML crawling mode.

Notes:

- The manifest does not cover every selected path (for example `/115`, `/ISO` and `/PikPak`). Such paths are skipped with a warning and their previously downloaded files are kept; run with `--force-crawl` periodically if you need them in sync via crawling, or use a full rebuild which crawls uncovered roots from two sources.
- A path that the manifest used to cover but no longer lists is treated as removed only after it stays absent across two consecutive manifest generations; until then its files are protected from cleanup.
- A file that remains unavailable after its retries (including `404` from every manifest mirror) is ignored for that round: its existing cache and media state are kept while the remaining files continue into the media phase. It is retried on the next trigger and appears under `本轮忽略` or `远端不存在` on the status page.
- Cleanup is disabled unless at least two mirrors serving the exact newest generation return identical manifest content. It is also disabled for malformed manifests and refuses to remove more than half of the local library or any root with at least 20 files.
- Deletions are crash-safe: files are quarantined into `.trash/` before database rows are removed. On restart, quarantined files whose rows still exist (including the pending full-rebuild snapshot) are restored; only committed deletions are discarded. Success markers are written last.
- Every `files` row carries its own time base (`manifest`, `http`, or `unknown`), a content identity (`etag:size` for strong ETags, otherwise a unique materialization ID) and a provenance. Timestamps are never compared across time bases: a row on a foreign base must first be re-identified via a current HTTP observation (equal size plus equal strong ETag) before its timestamp may be rewritten; otherwise the file is re-downloaded. Rows migrated from older versions start as `unknown`, so the first round after upgrading may issue extra HEAD observations and conservatively re-copy media files that lack strong ETags.
- Alist verification (purge) is fail-closed: any error other than an explicit not-found fails the whole compare phase, and only complete, successful listings may drop strm targets. Pagination must make forward progress and is bounded.
- Manifest/file paths are accessed through Go's rooted filesystem API, so symbolic links cannot escape the configured download or media trees. Individual files, complete rounds, manifest paths and manifest entry counts are bounded to limit hostile-mirror resource use.
- SQLite uses rollback journaling rather than WAL for compatibility with common NFS/SMB media volumes; downloaded file metadata is persisted by one batched writer, and read cursors are always fully drained before write transactions run.
- `--force-crawl` disables the manifest mode entirely and restores the legacy crawling behavior.
- `--download-workers` raises the download concurrency (default is the minimum of CPU count and 8; maximum 64). Keep it modest to stay friendly to the mirrors.

### Status Page & Web Control

The program serves a status page on `--listen-addr` (default `127.0.0.1:9527`, set the flag to `""` to disable). It shows the current sync phase, download progress with ETA, the mirror pool, recent sync-round history, and the last 1000 log lines, polling `/api/status` and `/api/logs` every 2 seconds.

In daemon mode the page also offers manual controls and a sync-settings editor:

- **Trigger a sync** — choose `incremental` (default), `full-relaxed` or `full-strict`. Strict full rebuilds require a second confirmation in a dialog and a `confirm: true` in the API request. Only one job runs at a time; additional triggers return `409 busy`.
- **Abort the running job** — cooperative cancellation: download dispatch, network requests and backoff waits stop; already-committed progress is kept.
- **Edit sync settings** — the two mode behavior bits (download cache / media library), cron, cleanup, purge, force-crawl, download workers, mirror list, Alist URL, strm root path and both skip-verify lists. Saves use optimistic revision locking (a stale revision returns `409 revision_conflict`). "恢复启动配置" deletes the persisted override.
- **Pending recovery** — when a full rebuild was interrupted, its state machine survives restarts; any later round first resumes it (the requested mode is overridden and the page says so). Disabling the download stage while a rebuild is pending pauses the recovery and blocks the media stage; re-enabling it lets the next scheduled or manual trigger resume.

Settings changed while a round is running only take effect on the next round; a cron change immediately reschedules the next idle run.

#### Persisted settings precedence

Web-edited settings are persisted to `downloadDir/.xiaoya-emby.json` (schema-versioned, written atomically with `0600`) and override the corresponding CLI/env values from the next start. `--daemon`, directories, listen address, log level, `*-from-file` sources and the control token remain startup-level only. A corrupt or schema-incompatible file is ignored (the startup baseline stays in force) and the status page keeps a visible warning until the next successful save or reset.

#### Control API authentication

- `--control-token` (env `CONTROL_TOKEN`) enables Bearer-token auth for the control API (`GET/PUT/DELETE /api/config`, `POST /api/sync`, `POST /api/sync/abort`), checked in constant time. Without a token the page is strictly read-only and rejects every control call with `403`, regardless of listen address or client origin — a same-host reverse proxy therefore never becomes an unauthenticated control channel.
- `/api/status` and `/api/logs` are intentionally unauthenticated even when a control token is configured. They expose sync state and recent logs, so never publish this listener directly to the public Internet.
- Write requests additionally require the `X-Requested-With: xiaoya-emby` header (CSRF protection) and `application/json` bodies with a 64 KiB limit.
- The token travels in cleartext HTTP headers: **on a non-loopback network you must put the page behind a TLS-terminating reverse proxy**, or the token (and thus control of your sync) can be intercepted. The server itself never speaks TLS.

#### Upgrade notes

- Stop the container before backing up SQLite state, then save `/download/.xiaoya-emby.json`, `/download/.metadata.db` and `/media/.metadata.db` if present. Pull the new image, remove the old container, and recreate it with the same mounts, ports, environment and arguments; `docker restart` does not replace the image.
- Removing the container does not remove bind-mounted data. To uninstall, remove only the container and any state files you intentionally no longer need. Do not delete the media library directory as part of uninstalling this utility.
- The `files` table gains three columns (`time_base`, `content_id`, `provenance`). The first start after the upgrade migrates existing databases automatically (batched backfill); **do not roll back to an older binary afterwards** — the old positional `INSERT` statements are incompatible with the migrated schema.
- Skip-verify prefixes (`--strm-path-skip-verify` / `--alist-path-skip-verify`) only narrow media verification. They never change the set of synced roots; cleanup and strict rebuilds always operate on the built-in root set.

### Full Rebuild Modes

Manual full rebuilds reconstruct the download cache (`downloadDir` and its `.metadata.db` `files` index) from scratch and then continue with the normal media compare/copy. The media library is never cleared.

- **`full-relaxed`** — first stages an authoritative inventory (see below), snapshots the old file list, then for every entry decides via an identity `HEAD` (`Accept-Encoding: identity`) whether the local file can be reused: same size, a valid mirror timestamp, and a local mtime (second precision) **not older** than the mirror time. Matching files are kept and re-recorded; everything else is downloaded. This is a deliberate performance/integrity trade-off: bytes may differ despite matching size/mtime, so this mode trusts the heuristic you chose.
- **`full-strict`** — after staging the inventory, the configured sync roots below `downloadDir` are cleared (single-level, normalized, reserved names like `.metadata.db`, `.xiaoya-emby.json` and the quarantine tree are never touched; symlinks are removed as links) and every entry is downloaded with identity encoding. Strict mode guarantees freshly fetched bytes.

Both modes are fail-closed while staging: at least two distinct mirrors must agree byte-for-byte on the newest manifest (zero malformed lines), and every root not covered by the manifest (e.g. `/115`, `/ISO`, `/PikPak`) is traversed independently on two crawl mirrors with the union taken. A root that is missing or empty on a mirror contributes an empty set instead of failing the full rebuild; malformed listings and other directory errors still fail the round before live data is touched. `force-crawl` applies the dual-source crawl to all roots. Rounds whose authoritative conditions are unmet end as `deferred` and wait for the next trigger instead of busy-retrying.

Interrupted rebuilds (crash or abort) are resumable: a persistent `full_sync_state` records the stable sync ID, the current phase (`clearing`/`downloading`/`rebuilding`) and the authoritative inventory. Every resume rebuilds the inventory against the latest mirror generation, re-verifies already-completed rows via HEAD and keeps the sync ID, so conclusions from an older generation are never mixed in. Quarantined files are reconciled against both the live rows and the pending snapshot before anything is deleted. Completion commits atomically (files count must equal the accepted inventory count exactly). Manifest entries still failing after bounded retries are ignored so successful entries continue to media; existing media is preserved, relaxed mode also preserves an old cache copy, and manifest short-circuit markers are withheld so the next incremental round retries them. A failed crawl-sourced entry also allows successful entries into media but leaves the rebuild deferred and resumable because ordinary manifest sync cannot rediscover that path. Staging rows are garbage-collected in small batches during idle windows. Deletion diffs are quarantined (strict always; relaxed only with `cleanup` enabled, otherwise files simply become untracked).

Rows without a strong ETag get a unique per-materialization identity: a relaxed reuse without a strong ETag still skips the network, but the changed identity forces one conservative media copy before later rounds can short-circuit again.

### Advanced Usage

Due to access rate limitations in the 115 cloud API, the program may mistakenly identify the target resource as inaccessible during scanning. Therefore, you can choose to skip the verification of those 115 media directories. The skipped media files will be automatically marked as valid.

If you are using the [Classic Installation](https://github.com/xiaoyaDev/xiaoya-alist), please refer to the table below for the paths that need to be ignored.

|Type|Path|
|-|-|
|Strm|`/115`|
|Alist|`/动漫/合集（115）`|
|Alist|`/每日更新/动漫/115合集-1`|
|Alist|`/每日更新/动漫/115合集-2`|
|Alist|`/每日更新/动漫/115合集-3`|
|Alist|`/每日更新/动漫/115合集-4`|
|Alist|`/每日更新/动漫/115合集-5`|
|Alist|`/🏷️我的115分享`|
|Alist|`/🏷️我的115`|

If you are deploying with containers, simply add the following startup parameters:

```text
--strm-path-skip-verify /115 --alist-path-skip-verify /动漫/合集（115）\
  --alist-path-skip-verify /每日更新/动漫/115合集-1 --alist-path-skip-verify /每日更新/动漫/115合集-2 \
  --alist-path-skip-verify /每日更新/动漫/115合集-3 --alist-path-skip-verify /每日更新/动漫/115合集-4 \
  --alist-path-skip-verify /每日更新/动漫/115合集-5 --alist-path-skip-verify /🏷️我的115分享 \
  --alist-path-skip-verify /🏷️我的115
```

For example:

```bash
docker run -d --name xiaoya-emby --restart unless-stopped \
  -v ${MY_DOWNLOAD_FOLDER}:/download -v ${MY_MEDIA_FOLDER}:/media \
  universonic/xiaoya-emby \
  --strm-path-skip-verify /115 --alist-path-skip-verify /动漫/合集（115）\
  --alist-path-skip-verify /每日更新/动漫/115合集-1 --alist-path-skip-verify /每日更新/动漫/115合集-2 \
  --alist-path-skip-verify /每日更新/动漫/115合集-3 --alist-path-skip-verify /每日更新/动漫/115合集-4 \
  --alist-path-skip-verify /每日更新/动漫/115合集-5 --alist-path-skip-verify /🏷️我的115分享 \
  --alist-path-skip-verify /🏷️我的115
```

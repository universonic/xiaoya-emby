# Xiaoya Utility for Emby

Utility to maintain metadata files in xiaoya media library for Emby.

It is an alternative to the `xiaoya-emd` utility, with boosted performance and emhanced feature.

### Build

Build for all platforms:

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

Golang 1.24.x is required.

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
      --cron-expr string                          Cron expression as scheduled task. Must run as daemon. (default "0 0 * * *")
      --daemon                                    Run as daemon in foreground. (default true)
  -D, --download-dir string                       Media directory of Emby to download metadata to. (default "/download")
      --download-workers int                      Number of concurrent download workers. 0 means auto (min(CPU, 8)).
      --force-crawl                               Force HTML crawling mode instead of manifest-based metadata sync.
  -h, --help                                      Print this message.
      --listen-addr string                        Address for the read-only status page (progress and logs). Set to "" to disable; expose beyond localhost only on trusted networks. (default "127.0.0.1:9527")
  -l, --log-level string                          Minimum log level (debug, info, warn, error). Env: LOG_LEVEL. (default "info")
  -d, --media-dir string                          Media directory of Emby to maintain metadata. (default "/media")
  -m, --mirror-url strings                        Specify the mirror URL to sync metadata from.
      --mode int                                  Run mode (4: scan metadata, 2: preserved bit, 1: sync metadata) (default 7)
  -p, --purge                                     Whether to purge useless file or directory when media is no longer available. (default true)
      --strm-path-skip-verify strings             Specify the metadata path to skip verify strm files. For example: "/115".
      --strm-path-skip-verify-from-file string    A file contains a list of strm path to skip verify.
  -v, --version                                   Print software version.
```

### Kickstart

This software requires a download folder and a media folder. It downloads metadata from mirrors, and modify the URLs in `.strm` files (if necessary, specified by `-r` and `-u`), then copy them to media folder. You should expose the media folder to your Emby server.

Simply start your container with:

```bash
docker run -d --name xiaoya-emby -v ${MY_DOWNLOAD_FOLDER}:/download -v ${MY_MEDIA_FOLDER}:/media universonic/xiaoya-emby
```

Enjoy!

### Metadata Sync Modes

By default the program syncs metadata via the pre-built manifest (`/.scan.list.gz`) published by the mirrors: all mirrors are probed concurrently, only the ones serving exactly the newest manifest generation are used, and the whole download phase is skipped when the current manifest generation (compared by Last-Modified and content hash) matches the last processed one. Identical content under a different timestamp is detected via hash and skipped as well; such no-op rounds still run a small incremental local integrity check that repairs missing files. When the manifest is unavailable on every mirror, the program automatically falls back to the legacy HTML crawling mode.

Notes:

- The manifest does not cover every selected path (for example `/115`, `/ISO` and `/PikPak`). Such paths are skipped with a warning and their previously downloaded files are kept; run with `--force-crawl` periodically if you need them in sync via crawling.
- A path that the manifest used to cover but no longer lists is treated as removed only after it stays absent across two consecutive manifest generations; until then its files are protected from cleanup.
- Cleanup is disabled unless at least two mirrors serving the exact newest generation return identical manifest content. It is also disabled for malformed manifests and refuses to remove more than half of the local library or any root with at least 20 files.
- Deletions are crash-safe: files are quarantined into `.trash/` before database rows are removed. On restart, quarantined files whose rows still exist are restored; only committed deletions are discarded. Success markers are written last.
- On the first manifest-based run after an upgrade, existing records are migrated to the manifest time base in-place, without re-downloading unchanged files. The media library switches its time base only after a successful copy phase, so a crash mid-copy keeps the conservative comparison rules in force.
- Manifest/file paths are accessed through Go's rooted filesystem API, so symbolic links cannot escape the configured download or media trees. Individual files, complete rounds, manifest paths and manifest entry counts are bounded to limit hostile-mirror resource use.
- SQLite uses rollback journaling rather than WAL for compatibility with common NFS/SMB media volumes; downloaded file metadata is persisted by one batched writer.
- `--force-crawl` disables the manifest mode entirely and restores the legacy crawling behavior.
- `--download-workers` raises the download concurrency (default is the minimum of CPU count and 8; maximum 64). Keep it modest to stay friendly to the mirrors.

### Status Page

The program serves a read-only status page on `--listen-addr` (default `127.0.0.1:9527`, set the flag to `""` to disable). It shows the current sync phase (manifest/crawl mode, probing, downloading, comparing, copying, cleanup, sleeping), download progress with ETA, the mirror pool with freshness and latency, recent sync-round history, and the last 1000 log lines. The page polls `/api/status` and `/api/logs` every 2 seconds. Both endpoints are unauthenticated and read-only, and mirror URLs are shown with credentials stripped; expose the port beyond localhost only on networks you trust.

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

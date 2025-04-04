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
      --alist-path-skip-verify strings   Specify the Alist path to skip verify files. For example: "/🏷️我的115分享"
  -r, --alist-strm-root-path string      Root path of strm files in xiaoya Alist (default "/d")
  -u, --alist-url string                 Endpoint of xiaoya Alist. Change this value will result to url overide in strm file (default "http://xiaoya.host:5678")
      --cron-expr string                 Cron expression as scheduled task. Must run as daemon. (default "0 0 * * *")
      --daemon                           Run as daemon in foreground (default true)
  -D, --download-dir string              Media directory of Emby to download metadata to (default "/download")
  -h, --help                             Print this message
  -d, --media-dir string                 Media directory of Emby to maintain metadata (default "/media")
  -m, --mirror-url strings               Specify the mirror URL to sync metadata from
      --mode int                         Run mode (4: scan metadata, 2: preserved bit, 1: sync metadata) (default 7)
  -p, --purge                            Whether to purge useless file or directory when media is no longer available (default true)
      --strm-path-skip-verify strings    Specify the metadata path to skip verify strm files. For example: "/115"
  -v, --version                          Print software version
```

### Kickstart

This software requires a download folder and a media folder. It downloads metadata from mirrors, and modify the URLs in `.strm` files (if necessary, specified by `-r` and `-u`), then copy them to media folder. You should expose the media folder to your Emby server.

Simply start your container with:

```bash
docker run -d --name xiaoya-emby -v ${MY_DOWNLOAD_FOLDER}:/download -v ${MY_MEDIA_FOLDER}:/media universonic/xiaoya-emby
```

Enjoy!

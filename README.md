# Xiaoya Utility for Emby

Utility to maintain metadata files in xiaoya media library for Emby.

It is an alternative to the `xiaoya-emd` utility, with boosted performance and more feature.

## Build

```bash
make
```

Golang 1.24.x is required.

## Usage (Command-Line)

```txt
Utility to maintain metadata files in xiaoya media library for Emby

Usage:
  xiaoya-emby [flags]

Flags:
  -r, --alist-strm-root-path string   Root path of strm files in xiaoya Alist (default "/d")
  -u, --alist-url string              Endpoint of xiaoya Alist. Change this value will result to url overide in strm file (default "http://xiaoya.host:5678")
      --daemon                        Run as daemon in foreground (default true)
  -D, --download-dir string           Media directory of Emby to download metadata to (default "/download")
  -h, --help                          Print this message
  -d, --media-dir string              Media directory of Emby to maintain metadata (default "/media")
  -m, --mirror-url string             Specify the mirror URL to sync metadata from
      --mode int                      Run mode (4: scan metadata, 2: scan alist, 1: sync metadata) (default 7)
  -p, --purge                         Whether to purge useless file or directory when media is no longer available (default true)
      --run-interval-in-hour int      Hours between two run cycles. Ignored unless run as daemon. (default 24)
```

## Kickstart

This software requires a download folder and a media folder. It downloads metadata from mirrors, and modify the URLs in `.strm` files (if necessary, specified by `-r` and `-u`), then copy them to media folder. You should expose the media folder to your Emby server.

Simply start your container with:

```bash
docker run -d --name xiaoya-emby -v ${MY_DOWNLOAD_FOLDER}:/download -v ${MY_MEDIA_FOLDER}:/media universonic/xiaoya-emd
```

Enjoy!
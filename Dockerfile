FROM golang:1.24 AS build

WORKDIR /app

COPY . .
RUN make


FROM --platform=linux/arm64/v8 almalinux/9-minimal AS arm64

WORKDIR /app

COPY --from=build /app/bin/xiaoya-emby-linux-arm64 /app/bin/xiaoya-emby
COPY entrypoint.sh /app/entrypoint.sh

ENV ALIST_STRM_ROOT_PATH="/d"
ENV ALIST_URL="http://xiaoya.host:5678"
ENV RUN_INTERVAL_IN_HOUR="24"

VOLUME /download
VOLUME /media

ENTRYPOINT [ "/app/entrypoint.sh" ]


FROM --platform=linux/amd64 almalinux/9-minimal AS amd64

WORKDIR /app

COPY --from=build /app/bin/xiaoya-emby-linux-amd64 /app/bin/xiaoya-emby
COPY entrypoint.sh /app/entrypoint.sh

ENV ALIST_STRM_ROOT_PATH="/d"
ENV ALIST_URL="http://xiaoya.host:5678"
ENV RUN_INTERVAL_IN_HOUR="24"

VOLUME /download
VOLUME /media

ENTRYPOINT [ "/app/entrypoint.sh" ]
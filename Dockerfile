FROM golang:1.25.1 AS build

ARG TARGETARCH

WORKDIR /app

COPY . .
RUN CGO_ENABLED=1 make "linux-${TARGETARCH}" && \
    cp "bin/xiaoya-emby-linux-${TARGETARCH}" /xiaoya-emby

FROM debian:trixie-slim

RUN groupadd -g 568 apps && \
    useradd -m -u 568 -g apps apps

USER apps

WORKDIR /app

COPY --from=build /xiaoya-emby /app/bin/xiaoya-emby
COPY entrypoint.sh /app/entrypoint.sh

ENV ALIST_STRM_ROOT_PATH="/d"
ENV ALIST_URL="http://xiaoya.host:5678"
ENV RUN_CRON_EXPR="0 0 * * *"

VOLUME /download
VOLUME /media

ENTRYPOINT [ "/app/entrypoint.sh" ]

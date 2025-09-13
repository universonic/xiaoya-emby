FROM golang:1.25 AS build

WORKDIR /app

COPY . .
RUN make


FROM --platform=linux/arm64 almalinux/9-minimal AS arm64

RUN groupadd -g 568 apps && \
    useradd -m -u 568 -g apps apps

USER apps

WORKDIR /app

COPY --from=build /app/bin/xiaoya-emby-linux-arm64 /app/bin/xiaoya-emby
COPY entrypoint.sh /app/entrypoint.sh

ENV ALIST_STRM_ROOT_PATH="/d"
ENV ALIST_URL="http://xiaoya.host:5678"
ENV RUN_CRON_EXPR="0 0 * * *"

VOLUME /download
VOLUME /media

ENTRYPOINT [ "/app/entrypoint.sh" ]


FROM --platform=linux/amd64 almalinux/9-minimal AS amd64

RUN groupadd -g 568 apps && \
    useradd -m -u 568 -g apps apps

USER apps

WORKDIR /app

COPY --from=build /app/bin/xiaoya-emby-linux-amd64 /app/bin/xiaoya-emby
COPY entrypoint.sh /app/entrypoint.sh

ENV ALIST_STRM_ROOT_PATH="/d"
ENV ALIST_URL="http://xiaoya.host:5678"
ENV RUN_CRON_EXPR="0 0 * * *"

VOLUME /download
VOLUME /media

ENTRYPOINT [ "/app/entrypoint.sh" ]
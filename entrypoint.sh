#!/bin/bash

ALIST_STRM_ROOT_PATH=${ALIST_STRM_ROOT_PATH:-"/d"}
ALIST_URL=${ALIST_URL:-"http://xiaoya.host:5678"}
RUN_CRON_EXPR=${RUN_CRON_EXPR:-"0 0 * * *"}


/app/bin/xiaoya-emby -r ${ALIST_STRM_ROOT_PATH} -u ${ALIST_URL} --cron-expr ${RUN_CRON_EXPR} "$@"
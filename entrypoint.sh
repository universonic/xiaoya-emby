#!/bin/bash

ALIST_STRM_ROOT_PATH=${ALIST_STRM_ROOT_PATH:-"/d"}
ALIST_URL=${ALIST_URL:-"http://xiaoya.host:5678"}
RUN_INTERVAL_IN_HOUR=${RUN_INTERVAL_IN_HOUR:-"24"}


/app/bin/xiaoya-emby -r ${ALIST_STRM_ROOT_PATH} -u ${ALIST_URL} --run-interval-in-hour ${RUN_INTERVAL_IN_HOUR} "$@"
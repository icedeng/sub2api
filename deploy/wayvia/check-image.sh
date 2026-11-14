#!/bin/sh
# 无凭据、无网络、无挂载地验证候选镜像；本脚本不发布或停止服务。
set -eu
image=${1:?usage: sh deploy/wayvia/check-image.sh registry/image@sha256:digest}
case "$image" in
  *@sha256:*) ;;
  *) echo '必须提供已构建并拉取的不可变镜像摘要' >&2; exit 1 ;;
esac
docker image inspect "$image" >/dev/null
capability=$(docker run --rm --network none --read-only --cap-drop ALL \
  --security-opt no-new-privileges:true --user 1000:1000 \
  --entrypoint /app/sub2api "$image" --wayvia-capabilities)
test "$capability" = 'wayvia-egress-v1' || { echo '镜像不含要求的出口扩展' >&2; exit 1; }
echo '镜像能力检查通过；仍需按README核对数据库版本、停流和单活。'

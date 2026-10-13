# docker build --build-arg BASE=ghcr.io/versolauth/versola-tools:0.6.2 \
#   --build-arg VERSION=1.0.1-e2e -t ghcr.io/versolauth/versola-tools:1.0.1-e2e .
# The tag is what `versola rollout 1.0.1-e2e` asks for: the CLI runs it from
# the local image store, it is never in a registry.
ARG BASE
FROM ${BASE}
ARG VERSION
ENV E2E_VERSION=${VERSION}
USER root
COPY --chmod=755 e2e-entrypoint.sh /e2e-entrypoint.sh
ENTRYPOINT ["/e2e-entrypoint.sh"]

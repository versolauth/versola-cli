# One stub image per service and version: docker build --build-arg ROLE=auth
# --build-arg VERSION=1.0.1-e2e -t versola-e2e/auth:1.0.1-e2e .
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /stub .

FROM scratch
ARG ROLE
ARG VERSION
# Seconds /readiness answers 503 after start (the stub's own default is 2).
ARG READY_DELAY=2
ENV STUB_ROLE=${ROLE} STUB_VERSION=${VERSION} STUB_READY_DELAY=${READY_DELAY}
COPY --from=build /stub /stub
ENTRYPOINT ["/stub"]

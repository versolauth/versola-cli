# One stub image per service and version: docker build --build-arg ROLE=auth
# --build-arg VERSION=1.0.1-e2e -t versola-e2e/auth:1.0.1-e2e .
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /stub .

FROM scratch
ARG ROLE
ARG VERSION
ENV STUB_ROLE=${ROLE} STUB_VERSION=${VERSION}
COPY --from=build /stub /stub
ENTRYPOINT ["/stub"]

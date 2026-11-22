# Build from the root of the allowlisted release snapshot.
FROM docker.io/library/golang:1.26.0-bookworm@sha256:2a0ba12e116687098780d3ce700f9ce3cb340783779646aafbabed748fa6677c AS build
ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local
WORKDIR /build/services
COPY services/ ./
RUN sha256sum go.mod go.sum > /tmp/go-dependencies.sha256 \
    && go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w" -o /out/platform ./cmd/platform \
    && sha256sum -c /tmp/go-dependencies.sha256

FROM docker.io/library/debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
ENV HOME=/home/miaoqia \
    MIAOQIA_DEPLOY_DIR=/opt/miaoqia/deploy
RUN groupadd --gid 10001 miaoqia \
    && useradd --uid 10001 --gid 10001 --create-home --no-log-init --shell /usr/sbin/nologin miaoqia \
    && chage --lastday 0 miaoqia
WORKDIR /opt/miaoqia
COPY --from=build /out/platform /usr/local/bin/platform
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY deploy/subsystems.yaml ./deploy/
COPY deploy/acquisition/roles.yaml ./deploy/acquisition/
COPY deploy/data/roles.yaml ./deploy/data/
COPY deploy/platform/roles.yaml ./deploy/platform/

RUN chmod -R a+rX,go-w deploy
USER 10001:10001
CMD ["platform", "-h"]

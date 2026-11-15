# TailLAN container image: shares a game server running in another
# container (or on the same machine) with friends. Its default command
# is "taillan host", configured with TAILLAN_* environment variables or
# arguments. See docs/server.md for compose, Kubernetes and more.
#
#   docker build -t taillan .
#   docker run --rm --network container:<game-server> -v taillan:/data taillan host valheim --keep
#
# Everything TailLAN keeps (the name friends see, kept invite keys,
# the API records "taillan status" reads) lives in /data. Mount a volume
# there to keep the same invite code across restarts.

# Build on the build machine's own architecture and cross-compile:
# TailLAN is pure Go, so no emulation is needed for other platforms.
FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:5e8afd493a7124a8586e134d9b532731161d4b9becf9dee5a5956b107426f93d AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev
# Link in the invite message hosts send ("Get TailLAN: ..."), as in
# release builds.
ARG DOWNLOAD_URL=
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -tags "$(cat build-tags.txt)" \
      -ldflags "-s -w -X main.version=$VERSION -X 'github.com/LaneBirmingham/taillan/internal/ui.DownloadURL=$DOWNLOAD_URL'" \
      -o /out/taillan . \
 && mkdir -p /out/data

# distroless/static: CA certificates (to reach the relays over TLS),
# a non-root user (65532) and nothing else, not even a shell.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
LABEL org.opencontainers.image.title="TailLAN" \
      org.opencontainers.image.description="Share a game server with friends over an encrypted tunnel: no port forwarding, no accounts" \
      org.opencontainers.image.source="https://github.com/LaneBirmingham/taillan" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/taillan /usr/local/bin/taillan
# A fresh named volume copies this directory's owner, so the non-root
# user can write to it.
COPY --from=build --chown=65532:65532 /out/data /data
ENV TAILLAN_HOME=/data
VOLUME /data
USER 65532:65532
# Healthy while a game is being shared (or joined and connected).
HEALTHCHECK --interval=30s --timeout=10s --start-period=60s \
  CMD ["/usr/local/bin/taillan", "status", "--quiet"]
ENTRYPOINT ["/usr/local/bin/taillan"]
CMD ["host"]

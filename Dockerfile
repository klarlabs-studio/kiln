# Kiln's own image: ghcr.io/klarlabs-studio/kiln
#
# Note what this image is *not*. It is not the artifact the Glossa dogfood
# produces, and it is not a worker image — kiln runs on a machine that already
# has git, warden, docker and cosign, because those are what it shells out to.
# Running kiln inside this container is for the HTTP surface (kilnd) and for
# operators who mount a socket and a toolchain in deliberately.

# Bases are pinned by digest the way Actions are pinned to a commit: a
# floating tag is a silent toolchain change. The tag stays so a reader can
# see which track this is; Dependabot (or a human) bumps the digest.
FROM golang:1.25-bookworm@sha256:3b4a11519ad929d1e1d261a12cff056f0c85b735253d7d861346b9c6f8b36437 AS build

WORKDIR /src

# Dependencies first: they change far less often than the source, so this layer
# survives almost every rebuild.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

# CGO off and a static link, because the runtime stage is distroless static and
# has no libc to dynamically link against.
ENV CGO_ENABLED=0
RUN go build -trimpath \
      -ldflags "-s -w \
        -X 'go.klarlabs.de/kiln/internal/version.Version=${VERSION}' \
        -X 'go.klarlabs.de/kiln/internal/version.Commit=${COMMIT}' \
        -X 'go.klarlabs.de/kiln/internal/version.Date=${DATE}'" \
      -o /out/kiln ./cmd/kiln \
 && go build -trimpath \
      -ldflags "-s -w \
        -X 'go.klarlabs.de/kiln/internal/version.Version=${VERSION}' \
        -X 'go.klarlabs.de/kiln/internal/version.Commit=${COMMIT}' \
        -X 'go.klarlabs.de/kiln/internal/version.Date=${DATE}'" \
      -o /out/kilnd ./cmd/kilnd

# nonroot: kiln has no reason to be root, and an image that builds other
# people's code is exactly the wrong place to make an exception.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

LABEL org.opencontainers.image.title="kiln" \
      org.opencontainers.image.description="Signed-artifact factory: prove a commit through warden, build it, sign the digest with cosign." \
      org.opencontainers.image.source="https://github.com/klarlabs-studio/kiln" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.authors="Felix Geelhaar <felix@felixgeelhaar.de>" \
      maintainer="Felix Geelhaar <felix@felixgeelhaar.de>"

COPY --from=build /out/kiln  /usr/local/bin/kiln
COPY --from=build /out/kilnd /usr/local/bin/kilnd

USER nonroot:nonroot
WORKDIR /workspace

EXPOSE 8088

# Distroless has no shell and no curl. The only check this image can run
# is that the binary we copied still starts. kilnd's real probe is
# GET /healthz from outside the container.
HEALTHCHECK --interval=30s --timeout=3s CMD ["/usr/local/bin/kiln", "version"]

ENTRYPOINT ["/usr/local/bin/kiln"]
CMD ["version"]

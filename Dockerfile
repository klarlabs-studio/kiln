# Kiln's own image: ghcr.io/klarlabs-studio/kiln
#
# Note what this image is *not*. It is not the artifact the Glossa dogfood
# produces, and it is not a worker image — kiln runs on a machine that already
# has git, warden, docker and cosign, because those are what it shells out to.
# Running kiln inside this container is for the HTTP surface (kilnd) and for
# operators who mount a socket and a toolchain in deliberately.

FROM golang:1.25-bookworm AS build

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
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="kiln" \
      org.opencontainers.image.description="Signed-artifact factory: prove a commit through warden, build it, sign the digest with cosign." \
      org.opencontainers.image.source="https://github.com/klarlabs-studio/kiln" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /out/kiln  /usr/local/bin/kiln
COPY --from=build /out/kilnd /usr/local/bin/kilnd

USER nonroot:nonroot
WORKDIR /workspace

EXPOSE 8088

ENTRYPOINT ["/usr/local/bin/kiln"]
CMD ["version"]

# syntax=docker/dockerfile:1
#
# FastRouter container image: static binary on distroless, runs as nonroot (uid 65532).
#
#   docker build -t fastrouter \
#     --build-arg VERSION=$(git describe --tags --always) \
#     --build-arg COMMIT=$(git rev-parse --short=12 HEAD) .
#
# BINDING: FastRouter listens on 127.0.0.1 by default, also inside the container.
# That is the container's own loopback, so a published port (-p) will NOT reach it.
# To use it from the host you must opt in to 0.0.0.0 inside the container, and should
# publish on the host's loopback only:
#
#   docker run -d --name fastrouter -p 127.0.0.1:20128:20128 -v fastrouter-data:/data \
#     fastrouter serve --config /data/fastrouter.json --host 0.0.0.0
#
# (or set "host": "0.0.0.0" in /data/fastrouter.json). No config, keys or secrets
# are baked into the image; everything lives in the /data volume.

ARG GO_VERSION=1.27.3

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=0.0.0-docker
ARG COMMIT=unknown
ARG DATE=unknown
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/zenpo/fastrouter/internal/server.Version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
      -o /out/fastrouter ./cmd/fastrouter \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/fastrouter /usr/local/bin/fastrouter
# Empty, nonroot-owned data dir; named volumes inherit this ownership.
COPY --from=build --chown=65532:65532 /out/data /data
USER nonroot:nonroot
VOLUME ["/data"]
EXPOSE 20128
ENTRYPOINT ["/usr/local/bin/fastrouter"]
CMD ["serve", "--config", "/data/fastrouter.json"]

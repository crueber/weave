# syntax=docker/dockerfile:1
# Base images are pinned by digest (tag in the comment). Bump with scripts/update-pins.sh, see docs/MAINTENANCE.md.
# golang:1.27
FROM golang:1.27@sha256:01aa404fa56b604b6f48e90c1d6e2c79fe5cd78f9592f78d80303fbe171cdcc0 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/crueber/weave/internal/domain/admin.Version=${VERSION}" \
    -o /out/weave ./cmd/weave
# distroless has no shell/mkdir: pre-create the data dir owned by nonroot (uid 65532)
RUN mkdir -p /out/data && chown 65532:65532 /out/data

# gcr.io/distroless/static-debian13:nonroot
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/weave /usr/local/bin/weave
COPY --from=build --chown=65532:65532 /out/data /data
ENV WEAVE_DATA_DIR=/data WEAVE_ADDR=:8080
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/usr/local/bin/weave", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/weave"]
CMD ["serve"]

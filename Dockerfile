# Build on the runner's native platform and cross-compile for the target, so a
# multi-arch build does not run the Go compiler under QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Stamped by the release pipeline; a plain `docker build` produces "(devel)"
# rather than claiming a version nobody cut.
ARG VERSION=(devel)
ARG COMMIT=unknown
ARG DATE=unknown

# Static binary so the runtime image needs no libc.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w \
      -X github.com/unifers/feather/internal/buildinfo.version=${VERSION} \
      -X github.com/unifers/feather/internal/buildinfo.commit=${COMMIT} \
      -X github.com/unifers/feather/internal/buildinfo.date=${DATE}" \
    -o /out/feather ./cmd/feather

FROM alpine:3.20
# ca-certificates for HTTPS to S3/OCI, tzdata because schedules carry a timezone.
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -u 10001 feather \
    && mkdir -p /var/lib/feather/spool \
    && chown -R feather:feather /var/lib/feather

COPY --from=build /out/feather /usr/local/bin/feather

USER feather
WORKDIR /var/lib/feather
# Everything else — accounts, retention configs, the administrator login —
# lives in the database under FEATHER_DATA_DIR. Mount it as a volume.
ENV FEATHER_DATA_DIR=/var/lib/feather \
    FEATHER_SPOOL_DIR=/var/lib/feather/spool \
    FEATHER_HTTP_ADDR=0.0.0.0:8477

VOLUME ["/var/lib/feather"]
EXPOSE 8477

ENTRYPOINT ["feather"]
CMD ["serve"]

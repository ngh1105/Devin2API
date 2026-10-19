# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/ngh1105/Devin2API/internal/buildinfo.Version=${VERSION}" \
      -o /out/devin2api ./cmd/devin2api \
 && mkdir -p /out/data

# distroless/static ships CA certificates (needed for the Devin API) and
# runs as the unprivileged "nonroot" user (uid 65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/devin2api /usr/local/bin/devin2api
COPY --from=build --chown=65532:65532 /out/data /data
ENV DEVIN2API_LISTEN=0.0.0.0:8080 \
    DEVIN2API_DATA=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/devin2api"]

# Build stage: pinned Go toolchain (by digest), locked dependencies (go.sum), static binary.
FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pr-code-stats ./cmd/pr-code-stats

# Runtime stage: git + CA certificates only. No USER directive: GitHub mounts the workspace and
# the file-command files with the runner's uid and requires Docker actions to run as the image's
# default user. safe.directory is scoped to the one path the runner mounts the workspace at.
# `apk upgrade` picks up security fixes published after the pinned base image was built.
FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN apk upgrade --no-cache \
 && apk add --no-cache git ca-certificates \
 && git config --system --add safe.directory /github/workspace
COPY --from=build /out/pr-code-stats /usr/local/bin/pr-code-stats
ENTRYPOINT ["/usr/local/bin/pr-code-stats"]

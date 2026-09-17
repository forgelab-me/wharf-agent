# Builds without a local Go toolchain: `docker build .` does everything.
# Versions pinned exactly on purpose (cf. ARCHITECTURE.md conventions) --
# bump deliberately, don't float on `latest`/`alpine`.
FROM golang:1.27.1-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# VERSION defaults to "dev" for a plain local `docker build .` -- CI passes
# the real one via --build-arg (git tag on a release, branch+sha otherwise).
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/wharf-agent .

FROM alpine:3.24.1
# docker-cli + docker-cli-compose: the agent shells out to `docker compose
# up` directly (Option C, no ephemeral container - cf. ARCHITECTURE.md,
# Flow de déploiement). Talks to the host's daemon over the mounted socket,
# doesn't need or run its own.
RUN apk add --no-cache ca-certificates docker-cli docker-cli-compose git openssh-client-default
COPY --from=build /out/wharf-agent /usr/local/bin/wharf-agent
# Static baseline for a plain local `docker build` -- CI's own --label
# flags (docker/metadata-action, cf. .github/workflows/agent.yml) take
# precedence and add version/created/revision, which only make sense
# coming from the actual build's git context.
LABEL org.opencontainers.image.title="wharf-agent" \
      org.opencontainers.image.description="Wharf agent -- executes deploys on a Docker host enrolled with a Wharf controller" \
      org.opencontainers.image.source="https://github.com/forgelab-me/wharf-agent" \
      org.opencontainers.image.licenses="MIT"
# Must survive container recreation: regenerating this identity would lose
# the fingerprint the controller already approved. Cf. ARCHITECTURE.md.
VOLUME /var/lib/wharf-agent/identity
# Where deployed stacks' compose files/env land - path parity with the
# host is required for any relative bind mount in a stack's compose file
# to resolve correctly (cf. ARCHITECTURE.md, compose-unpacker heritage).
VOLUME /opt/wharf-agent/stacks
ENTRYPOINT ["wharf-agent"]

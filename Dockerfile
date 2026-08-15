# Two binaries, one image: the API and the migrator.
#
# The migrator ships alongside the API deliberately -- migrations run as a
# discrete deploy step, and the step needs the exact migration set the new code
# expects. Shipping them separately is how a deploy ends up running last
# release's schema against this release's queries.

# Must match the toolchain go.mod declares. The golang images pin
# GOTOOLCHAIN=local, so unlike a dev machine the container will not silently
# download a newer toolchain -- it just fails the build.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first, so a code change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off and a static build, so the binaries run on distroless with no libc.
# -trimpath keeps build machine paths out of the binary; -s -w drops the symbol
# table and DWARF, which is most of the size.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api && \
    go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

# Distroless static: no shell, no package manager, nothing to pivot to if the
# process is ever compromised. Costs the ability to `docker exec` a shell,
# which is a fair trade for a service with an admin port for diagnostics.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/api /api
COPY --from=build /out/migrate /migrate

# Public API and admin (metrics, health). Only the first should ever be routed
# from outside.
EXPOSE 8080 9090

USER nonroot:nonroot
ENTRYPOINT ["/api"]

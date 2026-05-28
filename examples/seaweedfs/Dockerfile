# Minimal SeaweedFS build for outpost's source-canonical pipeline.
#
# The upstream seaweedfs/seaweedfs/docker/Dockerfile.go_build has a
# Rust stage that builds the alternative Rust-rewritten volume server
# — we deliberately skip it. The `weed` Go binary is the full server
# (master + volume + filer + S3 + admin in one process) and is all
# we need for our single-node v1 deployment.
#
# We build from a checked-out git ref rather than `go install
# .../weed@<ver>` because upstream renamed tags from "v3.97" style
# to plain "3.97" / "4.29" — and Go's module proxy requires the v
# prefix. Cloning lets us pin to whatever ref upstream actually
# publishes.
#
# Bump SEAWEEDFS_REF to upgrade. Default lags upstream by a release
# or two so we're not chasing prerelease bugs.
ARG SEAWEEDFS_REF=4.29

FROM golang:1.25-alpine AS builder
ARG SEAWEEDFS_REF
RUN apk add --no-cache git
RUN git clone --depth=1 --branch=${SEAWEEDFS_REF} \
        https://github.com/seaweedfs/seaweedfs /src
WORKDIR /src/weed
RUN CGO_ENABLED=0 go build -ldflags "-extldflags -static" -o /out/weed .

FROM alpine:3.23
RUN apk add --no-cache fuse ca-certificates
COPY --from=builder /out/weed /usr/bin/weed

# SeaweedFS canonical ports. vkpodman auto-allocates host-side ports
# at run time so the container-internal numbers stay as upstream
# documents them.
EXPOSE 9333 19333 8080 18080 8888 18888 8333

# vkpodman doesn't wire PVCs yet — /data is ephemeral overlay
# storage. v2 = real persistent volumes; for now, weed's data +
# indexes survive container restart but not pod recreation.
VOLUME /data
WORKDIR /data

ENTRYPOINT ["/usr/bin/weed"]
# `server -s3` runs the integrated stack (master + volume + filer +
# S3 gateway) in a single process — the right shape for our
# single-node v1 deployment. Split into per-component subcommands
# (`master` / `volume` / `filer`) once cross-outpost pod networking
# lets us spread roles across machines.
CMD ["server", "-s3", "-dir=/data"]

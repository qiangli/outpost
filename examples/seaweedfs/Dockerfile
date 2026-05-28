# Minimal SeaweedFS build for outpost's source-canonical pipeline.
#
# The upstream Dockerfile in seaweedfs/seaweedfs/docker/ has heavy
# Rust + multi-arch stages and re-clones the repo. We don't need any
# of that — `go install github.com/seaweedfs/seaweedfs/weed@<ver>`
# pulls source + builds the static Go binary in one step, off of a
# pinned upstream version, in ~3-5 min on an Apple Silicon outpost.
#
# Bump the SEAWEEDFS_VERSION build arg to upgrade. The default lags
# upstream by a release or two so we're not chasing prerelease bugs.
ARG SEAWEEDFS_VERSION=v3.97

FROM golang:1.25-alpine AS builder
ARG SEAWEEDFS_VERSION
RUN apk add --no-cache git
RUN CGO_ENABLED=0 go install -ldflags "-extldflags -static" \
        github.com/seaweedfs/seaweedfs/weed@${SEAWEEDFS_VERSION}

FROM alpine:3.23
RUN apk add --no-cache fuse ca-certificates
COPY --from=builder /go/bin/weed /usr/bin/weed

# Standard SeaweedFS ports. Volume server 8080 conflicts with whatever
# else the outpost might have running on the host loopback; vkpodman
# remaps via auto-allocated hostPorts so the container-side ports
# stay canonical.
EXPOSE 9333 19333 8080 18080 8888 18888 8333

# vkpodman doesn't wire PVCs yet — /data is ephemeral overlay storage.
# v2 = real persistent volumes; for now, "weed server" data + indexes
# survive the container restart but not pod recreation.
VOLUME /data
WORKDIR /data

ENTRYPOINT ["/usr/bin/weed"]
# `server -s3` runs the integrated stack (master + volume + filer + S3
# gateway) in a single process — the right shape for our single-node
# v1 deployment. Override to `volume` / `master` / `filer` once we
# split components across nodes.
CMD ["server", "-s3", "-dir=/data"]

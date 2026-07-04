# Mode-independent image: the SAME binary runs bare-metal, in podman, or as a
# DKS pod (the deployment-mode contract). Build once, inject env at run time.
#
#   docker build --build-arg VERSION=v0.1.0 --build-arg COMMIT=$(git rev-parse --short HEAD) -t hello-tessaro .
#   docker run -e APP_ENV=qa -e HELLO_TESSARO_SSO_SECRET=... -e HELLO_TESSARO_ADDR=0.0.0.0:8080 -p 8080:8080 hello-tessaro
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
# CGO off → a static binary that runs on distroless/scratch and cross-compiles cleanly.
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT}" \
      -o /out/hello-tessaro .

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/hello-tessaro /app/hello-tessaro
COPY --from=build /src/config.*.json /app/
# In a container the app is not loopback-only — outpost/cluster networking fronts
# it — so bind all interfaces here. HMAC stays required (see config.<env>.json).
ENV APP_ENV=prod HELLO_TESSARO_ADDR=0.0.0.0:8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/hello-tessaro"]

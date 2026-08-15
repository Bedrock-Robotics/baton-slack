# baton-slack runs in service mode: a long-lived process that holds an
# outbound-only connection to ConductorOne and syncs on an interval. It listens
# on no port, so the image exposes none.
#
# The image is single-architecture. TARGET_ARCH picks both the Go target and the
# runtime base, so the binary and the base can never disagree. The ECS task
# definition's runtimePlatform must name the same architecture. Neither side can
# see the other, and a mismatch surfaces as a task that fails to launch without
# naming architecture as the cause.
ARG TARGET_ARCH=arm64

# Go stays on the builder's own architecture and cross-compiles, so the build
# needs no emulation. Keep this in step with .versions.yaml and go.mod.
FROM --platform=$BUILDPLATFORM public.ecr.aws/docker/library/golang:1.25.2 AS build

WORKDIR /src
COPY . .

# No baton_lambda_support tag. That tag builds the other transport, where
# ConductorOne invokes a Lambda; we chose Fargate service mode.
ARG TARGET_ARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGET_ARCH} go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/baton-slack \
    ./cmd/baton-slack

# distroless static carries a CA bundle, an /etc/passwd with a nonroot user, and
# a writable /tmp for the sync file. The CA bundle is the reason to prefer it
# over scratch: without it every call to Slack and ConductorOne fails
# certificate verification, and the connector logs that as a network error.
FROM --platform=linux/${TARGET_ARCH} gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/baton-slack /usr/local/bin/baton-slack

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/baton-slack"]

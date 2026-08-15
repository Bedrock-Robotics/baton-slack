# TARGET_ARCH picks both the Go target and the runtime base, so the binary and
# the base cannot disagree. The ECS task definition's runtimePlatform must name
# the same architecture; a mismatch fails the task launch without naming
# architecture as the cause.
ARG TARGET_ARCH=arm64

FROM --platform=$BUILDPLATFORM public.ecr.aws/docker/library/golang:1.25.2 AS build

WORKDIR /src
COPY . .

ARG TARGET_ARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGET_ARCH} go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/baton-slack \
    ./cmd/baton-slack

# distroless static rather than scratch: it carries the CA bundle. Without it
# every call to Slack and ConductorOne fails certificate verification, which the
# connector reports as a network error.
FROM --platform=linux/${TARGET_ARCH} gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/baton-slack /usr/local/bin/baton-slack

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/baton-slack"]

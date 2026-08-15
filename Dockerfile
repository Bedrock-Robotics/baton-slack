FROM --platform=$BUILDPLATFORM public.ecr.aws/docker/library/golang:1.25.2 AS build

WORKDIR /src
COPY . .

# TARGETARCH names the platform being built for. The build stage stays on the
# builder's own platform and cross-compiles, so building several platforms at
# once needs no emulation. A plain `docker build` takes TARGETARCH from the
# host; pass --platform to get anything else.
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/baton-slack \
    ./cmd/baton-slack

# distroless static rather than scratch: it carries the CA bundle. Without it
# every call to Slack and ConductorOne fails certificate verification, which the
# connector reports as a network error.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/baton-slack /usr/local/bin/baton-slack

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/baton-slack"]

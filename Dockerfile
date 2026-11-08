# syntax=docker/dockerfile:1

# The webhook in a distroless image, run as a user without privileges. The
# build runs on the builder's platform and cross-compiles for the target's,
# so a multi-platform build needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/webhook .

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/webhook /webhook
# Numeric, so that a pod with runAsNonRoot can verify it (Kubernetes can't
# check a user given by name): distroless's nonroot is 65532.
USER 65532:65532
# 8888: the webhook API, on localhost only by default. 8080: /healthz and
# /readyz.
EXPOSE 8888 8080
ENTRYPOINT ["/webhook"]

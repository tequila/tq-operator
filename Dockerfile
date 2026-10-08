# tq-operator — one statically linked binary in a distroless image: no shell, no package
# manager, nothing to exec into.
# Both bases are pinned by digest (multi-arch indexes); the release workflow builds one leg per
# architecture on a native runner (linux/arm64, linux/amd64).

FROM golang:1.26-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/tq-operator ./cmd

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
LABEL org.opencontainers.image.source="https://github.com/tequila/tq-operator" \
      org.opencontainers.image.title="tq-operator" \
      org.opencontainers.image.description="The Tequila estate operator — Estate at rung observe"
COPY --from=build /out/tq-operator /tq-operator
USER 65532:65532
ENTRYPOINT ["/tq-operator"]

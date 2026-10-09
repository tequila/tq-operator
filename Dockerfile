# tq-operator — one statically linked binary in a distroless image: no shell, no package
# manager, nothing to exec into.
# The image is assembled, never compiled: `make dist` builds the static binaries
# (CGO_ENABLED=0, dist/tq-operator-linux-<arch>) and this file only copies the one of the
# target architecture. Nothing executes during the build, so one `docker buildx build
# --platform linux/amd64,linux/arm64` produces both legs on any runner, with no emulation.
# The base is pinned by digest (a multi-arch index).

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG TARGETARCH
LABEL org.opencontainers.image.source="https://github.com/tequila/tq-operator" \
      org.opencontainers.image.title="tq-operator" \
      org.opencontainers.image.description="The Tequila estate operator — Estate at rung observe"
COPY --chmod=0555 dist/tq-operator-linux-${TARGETARCH} /tq-operator
USER 65532:65532
ENTRYPOINT ["/tq-operator"]

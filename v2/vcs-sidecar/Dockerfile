FROM krancour/go-tools:v0.4.0
ARG VERSION
ARG COMMIT
ENV CGO_ENABLED=0
WORKDIR /src
COPY sdk/ sdk/
WORKDIR /src/v2
COPY v2/vcs-sidecar vcs-sidecar/
COPY v2/go.mod go.mod
COPY v2/go.sum go.sum

RUN cd vcs-sidecar && go build \
  -o ../../bin/vcs-sidecar \
  -ldflags "-w -X github.com/brigadecore/brigade/v2/vcs-sidecar/version.version=$VERSION -X github.com/brigadecore/brigade/v2/vcs-sidecar/version.commit=$COMMIT" \
  .

# FROM scratch
# TODO: go back to scratch; using alpine for testing (use of core utilities like 'ls', etc.)
FROM alpine
COPY --from=0 /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=0 /src/bin/ /brigade/bin/
ENTRYPOINT ["/brigade/bin/vcs-sidecar"]

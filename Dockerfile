# Multi-stage build: the dev host has Docker but no Go toolchain, so Go lives only in the
# build stage. px-smt-multirealm-shim uses the standard library only, so the build needs no module proxy.
FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath \
    go build -ldflags="-s -w" -o /px-smt-multirealm-shim ./cmd/px-smt-multirealm-shim

# scratch keeps the image tiny and dependency-free. No CA bundle is needed: the shim
# generates its own server cert and (phase 1) skips verification of the FlashArray cert.
FROM scratch
COPY --from=build /px-smt-multirealm-shim /px-smt-multirealm-shim
# Non-root UID; OpenShift's restricted SCC may override this with a random UID, which is fine.
USER 65532:65532
EXPOSE 9443
ENTRYPOINT ["/px-smt-multirealm-shim"]

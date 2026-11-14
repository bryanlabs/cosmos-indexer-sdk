# Builds replay033: the Cosmos Hub 1 application of 2019 (cosmos-sdk v0.33.2)
# with a driver that feeds it cached blocks and checks every state hash.
FROM golang:1.27.3-bookworm@sha256:dc0963f32053e4bc451d24d87e3892d42d01021b61f093e98eb95c0352bb84b0 AS build
WORKDIR /src
ENV GOTOOLCHAIN=local GOFLAGS=-mod=mod CGO_ENABLED=0
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,id=cosmos-tax-replay-go-mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod,id=cosmos-tax-replay-go-mod \
    --mount=type=cache,target=/root/.cache/go-build,id=cosmos-tax-replay-go-build \
    go test ./... && go build -trimpath -o /out/replay033 . && \
    go build -trimpath -o /out/checkpoint-proof ./cmd/checkpoint-proof

FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
RUN adduser -D -u 1137 tax
COPY --from=build /out/replay033 /usr/local/bin/replay033
COPY --from=build /out/checkpoint-proof /usr/local/bin/checkpoint-proof
USER tax
ENTRYPOINT ["/usr/local/bin/replay033"]

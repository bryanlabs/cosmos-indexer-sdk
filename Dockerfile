FROM golang:1.26-alpine AS build-env

# Customize to your build env

# TARGETPLATFORM should be one of linux/amd64 or linux/arm64
ARG TARGETPLATFORM

# Use muslc for static libs
ARG BUILD_TAGS=muslc
ARG LD_FLAGS=-linkmode=external -extldflags '-Wl,-z,muldefs -static'

# Install cli tools for building and final image
RUN apk add --update --no-cache curl make git libc-dev bash gcc linux-headers eudev-dev ncurses-dev libc6-compat jq htop atop iotop

# Install build dependencies.
RUN if [ "${TARGETPLATFORM}" = "linux/amd64" ] ; then \
      wget -P /lib https://github.com/CosmWasm/wasmvm/releases/download/v1.2.3/libwasmvm_muslc.x86_64.a ; \
      cp /lib/libwasmvm_muslc.x86_64.a /lib/libwasmvm_muslc.a ; \
    fi

RUN if  [ "${TARGETPLATFORM}" = "linux/arm64" ] ; then \
      wget -P /lib https://github.com/CosmWasm/wasmvm/releases/download/v1.2.3/libwasmvm_muslc.aarch64.a ; \
      cp /lib/libwasmvm_muslc.aarch64.a /lib/libwasmvm_muslc.a ; \
    fi

# Build main app.
WORKDIR /go/src/app
COPY . .
RUN if [ "${TARGETPLATFORM}" = "linux/amd64" ] ; then \
      GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go install -ldflags ${LD_FLAGS} -tags ${BUILD_TAGS} ; \
    fi

RUN if [ "${TARGETPLATFORM}" = "linux/arm64" ] ; then \
      GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go install -ldflags ${LD_FLAGS} -tags ${BUILD_TAGS} ; \
    fi

# Use busybox to create a user
FROM busybox:stable-musl AS busybox
RUN addgroup --gid 1137 -S cosmos-indexer && adduser --uid 1137 -S cosmos-indexer -G cosmos-indexer

# Use scratch for the final image
FROM scratch
WORKDIR /home/cosmos-indexer

# Label should match your github repo
LABEL org.opencontainers.image.source="https://github.com/defiantlabs/cosmos-indexer"

# Installs all binaries built with go.
COPY --from=build-env /go/bin /bin

# Other binaries we want to keep.
COPY --from=build-env /usr/bin/ldd /bin/ldd
COPY --from=build-env /usr/bin/curl /bin/curl
COPY --from=build-env /usr/bin/jq /bin/jq
COPY --from=build-env /usr/bin/htop /bin/htop
COPY --from=build-env /usr/bin/atop /bin/atop

# Install Libraries
# cosmos-indexer
COPY --from=build-env /usr/lib/libgcc_s.so.1 /lib/
COPY --from=build-env /lib/ld-musl*.so.1* /lib

# jq Libraries
COPY --from=build-env /usr/lib/libonig.so.5 /lib

# curl Libraries
COPY --from=build-env /usr/lib/libcurl.so.4 /lib
COPY --from=build-env /lib/libz.so.1 /lib
COPY --from=build-env /usr/lib/libnghttp2.so.14 /lib
COPY --from=build-env /lib/libssl.so.3 /lib
COPY --from=build-env /lib/libcrypto.so.3 /lib
COPY --from=build-env /usr/lib/libbrotlidec.so.1 /lib
COPY --from=build-env /usr/lib/libbrotlicommon.so.1 /lib

# htop/atop libs
COPY --from=build-env /usr/lib/libncursesw.so.6 /lib

# Install trusted CA certificates
COPY --from=build-env /etc/ssl/cert.pem /etc/ssl/cert.pem

# Install cli tools from busybox
COPY --from=busybox /bin/ln /bin/ln
COPY --from=busybox /bin/dd /bin/dd
COPY --from=busybox /bin/vi /bin/vi
COPY --from=busybox /bin/chown /bin/chown
COPY --from=busybox /bin/id /bin/id
COPY --from=busybox /bin/cp /bin/cp
COPY --from=busybox /bin/ls /bin/ls
COPY --from=busybox /bin/busybox /bin/sh
COPY --from=busybox /bin/cat /bin/cat
COPY --from=busybox /bin/less /bin/less
COPY --from=busybox /bin/grep /bin/grep
COPY --from=busybox /bin/sleep /bin/sleep
COPY --from=busybox /bin/env /bin/env
COPY --from=busybox /bin/tar /bin/tar
COPY --from=busybox /bin/tee /bin/tee
COPY --from=busybox /bin/du /bin/du
COPY --from=busybox /bin/df /bin/df
COPY --from=busybox /bin/nc /bin/nc
COPY --from=busybox /bin/netstat /bin/netstat

# Copy user from busybox to scratch
COPY --from=busybox /etc/passwd /etc/passwd
COPY --from=busybox --chown=1137:1137 /home/cosmos-indexer /home/cosmos-indexer

# Set home directory and user
WORKDIR /home/cosmos-indexer
RUN chown -R cosmos-indexer /home/cosmos-indexer
USER cosmos-indexer

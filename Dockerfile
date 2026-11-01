FROM golang:1.26.10-alpine3.24@sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308 AS build-env

# Customize to your build env

# TARGETPLATFORM should be one of linux/amd64 or linux/arm64
ARG TARGETPLATFORM

# Use muslc for static libs
ARG BUILD_TAGS=muslc
# wasmvm 1.2.3 references the former musl large-file aliases. On 64-bit
# targets they have the same ABI as the unsuffixed functions.
ARG LD_FLAGS="-linkmode=external -extldflags '-Wl,-z,muldefs -static -no-pie -Wl,--defsym,fstat64=fstat,--defsym,ftruncate64=ftruncate,--defsym,lseek64=lseek,--defsym,fstatat64=fstatat,--defsym,stat64=stat,--defsym,pread64=pread,--defsym,pwrite64=pwrite,--defsym,lstat64=lstat,--defsym,readdir64=readdir,--defsym,open64=open'"
ARG GOMAXPROCS=2
ARG GOFLAGS=-p=1

# Install cli tools for building and final image
RUN apk add --update --no-cache curl make git libc-dev bash gcc linux-headers eudev-dev ncurses-dev gcompat jq htop atop iotop

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
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    if [ "${TARGETPLATFORM}" = "linux/amd64" ] ; then \
      GOMAXPROCS="${GOMAXPROCS}" GOFLAGS="${GOFLAGS}" GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go install -ldflags "${LD_FLAGS}" -tags "${BUILD_TAGS}" ; \
    fi

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    if [ "${TARGETPLATFORM}" = "linux/arm64" ] ; then \
      GOMAXPROCS="${GOMAXPROCS}" GOFLAGS="${GOFLAGS}" GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go install -ldflags "${LD_FLAGS}" -tags "${BUILD_TAGS}" ; \
    fi

# Keep the runtime library closure of the diagnostic tools, including transitive
# dependencies, rather than maintaining a version-specific list of shared libs.
RUN mkdir /runtime-libs && \
    cp -L /usr/lib/libgcc_s.so.1 /runtime-libs/ && \
    for tool in /usr/bin/curl /usr/bin/jq /usr/bin/htop /usr/bin/atop; do \
      for lib in $(ldd "$tool" | awk '$2 == "=>" {print $3} $1 ~ /^\// {print $1}'); do \
        cp -L "$lib" /runtime-libs/; \
      done; \
    done

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

# Install the diagnostic tools' shared libraries and musl loader.
COPY --from=build-env /runtime-libs/ /lib/

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
RUN ["/bin/curl", "--version"]
RUN ["/bin/jq", "--version"]
RUN ["/bin/htop", "--version"]
RUN ["/bin/atop", "-V"]
USER cosmos-indexer
RUN ["/bin/cosmos-indexer", "--help"]

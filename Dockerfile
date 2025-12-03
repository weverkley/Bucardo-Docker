FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY . .
RUN go mod tidy
RUN go build -o /entrypoint ./cmd/app


FROM ubuntu:22.04

ARG BUCARDO_VERSION=5.6.0
ARG PG_VERSION=14

LABEL \
    maintainer="Wever Kley <wever-kley@live.com>" \
    org.opencontainers.image.title="Bucardo Docker Image" \
    org.opencontainers.image.description="An Ubuntu-based Docker image for Bucardo, a PostgreSQL replication system." \
    org.opencontainers.image.authors="Wever Kley <wever-kley@live.com>" \
    org.opencontainers.image.source="https://github.com/wever-kley/bucardo_docker_image" \
    org.opencontainers.image.documentation="https://github.com/wever-kley/bucardo_docker_image/blob/main/README.md" \
    org.opencontainers.image.licenses="Apache-2.0"

ENV DEBIAN_FRONTEND=noninteractive
ENV TZ=America/Sao_Paulo

RUN apt-get -y update

RUN apt-get -y install postgresql-client-${PG_VERSION} jq wget curl perl make build-essential bucardo && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /tmp

# It's possible the 'bucardo' package from apt is just a wrapper or an older version.
# The original Dockerfile installed from source, so we preserve that logic to be safe.
RUN wget -O /tmp/bucardo.tgz https://github.com/bucardo/bucardo/archive/refs/tags/${BUCARDO_VERSION}.tar.gz && \
    tar zxf /tmp/bucardo.tgz && \
    cd bucardo-${BUCARDO_VERSION} && \
    perl Makefile.PL && \
    make && \
    make install && \
    cd / && \
    rm -rf /tmp/bucardo.tgz /tmp/bucardo-${BUCARDO_VERSION}

# Copy the Go application binary from the builder stage
COPY --from=builder /entrypoint /entrypoint

# Copy the custom entrypoint script and make it executable
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# Set the working directory for the application
WORKDIR /media/bucardo

# Use the custom entrypoint script
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD []

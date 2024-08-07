# Build Geth in a stock Go builder container
FROM golang:1.22-alpine3.20 as builder

ENV GOPROXY=https://proxy.golang.org,direct

RUN apk update
RUN apk add --no-cache gcc g++ musl-dev linux-headers git ca-certificates openssl libstdc++

# Define the location for custom certificates
ARG cert_location=/usr/local/share/ca-certificates

# Fetch and install certificates for github.com and proxy.golang.org
RUN openssl s_client -showcerts -connect github.com:443 </dev/null 2>/dev/null | \
 openssl x509 -outform PEM > ${cert_location}/github.crt && \
 update-ca-certificates
RUN openssl s_client -showcerts -connect proxy.golang.org:443 </dev/null 2>/dev/null | \
 openssl x509 -outform PEM > ${cert_location}/proxy.golang.crt && \
 update-ca-certificates

WORKDIR /prysm

RUN mkdir -p /app/bin/

ADD . .
RUN CGO_CFLAGS="-O2 -D__BLST_PORTABLE__" go build -v -trimpath -o /app/bin ./cmd/prysmctl
RUN CGO_CFLAGS="-O2 -D__BLST_PORTABLE__" go build -v -trimpath -o /app/bin ./cmd/beacon-chain
RUN CGO_CFLAGS="-O2 -D__BLST_PORTABLE__" go build -v -trimpath -o /app/bin ./cmd/validator

FROM alpine:3.20

RUN apk add --no-cache ca-certificates curl libstdc++
COPY --from=builder /app/bin/* /usr/local/bin/
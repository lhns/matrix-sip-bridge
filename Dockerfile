# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build

# CGO is required, not optional: bridgev2's mxmain imports mattn/go-sqlite3
# unconditionally, so CGO_ENABLED=0 does not compile. The binary is still
# statically linked against musl, so it runs on distroless/static.
RUN apk add --no-cache build-base

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# The goolm tag selects mautrix's pure-Go Olm implementation. Without it a CGO
# build wants libolm headers, which are not in the base image.
RUN CGO_ENABLED=1 GOOS=linux go build \
        -tags goolm \
        -trimpath \
        -ldflags="-s -w -linkmode external -extldflags -static" \
        -o /matrix-sip-bridge .

# The audio check ships in the same image rather than one of its own: it needs no
# CGO and nothing the bridge does not already have, and a second image is a second
# tag to keep in step with this one.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -tags goolm \
        -trimpath \
        -ldflags="-s -w" \
        -o /callaudit ./cmd/callaudit

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /matrix-sip-bridge /matrix-sip-bridge
COPY --from=build /callaudit /callaudit
ENTRYPOINT ["/matrix-sip-bridge"]

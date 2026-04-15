FROM --platform=$BUILDPLATFORM golang:1.26.2-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/gridlane ./cmd/gridlane

FROM alpine:3.23

RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/gridlane /usr/bin/gridlane

USER 65532:65532
EXPOSE 4444 9090
ENTRYPOINT ["/usr/bin/gridlane"]

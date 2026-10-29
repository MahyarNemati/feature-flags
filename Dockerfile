# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/flagd ./cmd/flagd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/flagd /usr/local/bin/flagd
# The example flags make the image useful out of the box for a demo.
COPY --from=build /src/flags.example.json /etc/flagd/flags.example.json
USER nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/flagd"]

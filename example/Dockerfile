# syntax=docker/dockerfile:1

FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY . .
RUN cd example && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/example .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/example /example
EXPOSE 8080
ENTRYPOINT ["/example"]

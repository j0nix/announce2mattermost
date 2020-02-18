# First use Docker builder to build my go app
FROM golang:latest AS builder
WORKDIR /build
COPY . .
#RUN go mod download
RUN go mod download
RUN go build -tags=netgo -o app .

# When we no have our go binary built, build the actual Docker image
FROM scratch
COPY --from=builder /build/app /
COPY server.crt server.key /
COPY web /web
EXPOSE 8080

# Start the service.
ENTRYPOINT ["/app"]

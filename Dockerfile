FROM golang:1.26-alpine AS builder
WORKDIR /app

COPY . .
RUN go build -o registry .

FROM scratch
COPY --from=builder /app/registry /registry
EXPOSE 8080
ENTRYPOINT ["/registry"]
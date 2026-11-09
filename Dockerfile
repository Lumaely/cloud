FROM golang:1.27.1-alpine AS builder

WORKDIR /app

COPY go.sum go.mod ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-X main.version=${VERSION}" -o main .

FROM gcr.io/distroless/static

LABEL authors="Pierre Marguerie"

COPY --from=builder /app/main /main

ENTRYPOINT ["/main"]
CMD ["serve"]

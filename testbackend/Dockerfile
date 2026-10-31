FROM golang:1.26.10 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o backend \
    ./testbackend

    
FROM scratch

COPY --from=builder /src/backend /backend
    
EXPOSE 8080
    
ENTRYPOINT ["/backend"]
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -installsuffix cgo -o talos-proxmox-autoscaler .

FROM gcr.io/distroless/static-debian12

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/talos-proxmox-autoscaler /usr/local/bin/

EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["talos-proxmox-autoscaler"]

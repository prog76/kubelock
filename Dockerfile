FROM golang:1.23-alpine AS builder

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build . && \
    mv kubelock /usr/local/bin/

FROM alpine:3.13
COPY --from=builder /usr/local/bin/kubelock /usr/local/bin/kubelock

ENTRYPOINT ["kubelock"]

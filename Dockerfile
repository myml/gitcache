FROM golang as builder
COPY . /src
WORKDIR /src
RUN CGO_ENABLED=0 go build -o server

FROM debian
RUN apt-get update -y
RUN apt-get install -y git
COPY --from=builder /etc/ssl/certs /etc/ssl/certs
COPY --from=builder /src/server /
CMD ["/server"]
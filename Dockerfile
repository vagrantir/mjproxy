FROM golang:latest AS build

RUN mkdir -p /go/src/github.com/vagrantir/mjproxy
COPY . /go/src/github.com/vagrantir/mjproxy

WORKDIR /go/src/github.com/vagrantir/mjproxy

RUN go build

FROM alpine:3.7

EXPOSE 4000

# Optional volumes
# /data - used for persistent storage across reboots
# VOLUME /data
# /etc/ssl/certs - directory for SSL certificates
# VOLUME /etc/ssl/certs

COPY --from=build /opt/nsq/bin/ /usr/local/bin/
RUN ln -s /usr/local/bin/mjproxy / \
 && ln -s /usr/local/bin/mjproxy /bin/
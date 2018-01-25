FROM alpine
RUN apk update && apk add ca-certificates && rm -rf /var/cache/apk/*

ARG SPARTA_DOCKER_BINARY
ADD $SPARTA_DOCKER_BINARY /SpartaDocker
EXPOSE 80
CMD ["/SpartaDocker", "sqsWorker"]
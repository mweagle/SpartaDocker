FROM centurylink/ca-certs
ARG SPARTA_DOCKER_BINARY
ADD $SPARTA_DOCKER_BINARY /SpartaDocker
EXPOSE 80
CMD ["/SpartaDocker", "sqsWorker"]
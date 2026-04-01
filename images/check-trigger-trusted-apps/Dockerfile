FROM registry.access.redhat.com/ubi9/ubi-minimal:latest
LABEL maintainer="jguzik@redhat.com"

ADD check-trigger-trusted-apps /usr/bin/check-trigger-trusted-apps
ENTRYPOINT ["/usr/bin/check-trigger-trusted-apps"]

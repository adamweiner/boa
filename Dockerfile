FROM alpine:3.5

ADD ./ /go/src/github.com/adamweiner/boa

RUN apk update && \
    apk add -U build-base go git curl libstdc++ lame && \
    cd /go/src/github.com/adamweiner/boa && \
    GOPATH=/go make && \
    apk del build-base go git

CMD /go/src/github.com/adamweiner/boa

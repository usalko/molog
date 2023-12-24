FROM golang:1.21.0-alpine AS build

RUN apk add --update --no-cache alpine-sdk bash python3

# copy source files and private repo dep
COPY ./src/ /go/src/molog/
# COPY ./vendor/ /go/src/molog/vendor/

# static build the app
WORKDIR /go/src/molog
# RUN go mod init
RUN go mod tidy
RUN go install -tags=musl

RUN go build -tags=musl -tags=dynamic

# SHOW CONTENT FROM BUILD FOLDER
RUN ls -la /go/src/molog

# create final image
FROM alpine:3.18.3 AS runtime

COPY --from=build /go/src/molog/main /usr/bin/molog
COPY --from=build /usr/local /usr/local

# RUN apk --no-cache add \
#       cyrus-sasl \
#       openssl \

ENTRYPOINT ["molog"]

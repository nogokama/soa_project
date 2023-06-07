FROM golang:latest
WORKDIR /go/src/app
COPY . /go/src/app
RUN ls
RUN go build -o app ./server
ENTRYPOINT [ "./app" ]

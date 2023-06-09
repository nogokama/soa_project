FROM golang:latest
WORKDIR /go/src/app
COPY go.mod /go/src/app
RUN go mod download
COPY . /go/src/app
RUN go build -o app ./server
ENTRYPOINT [ "./app" ]

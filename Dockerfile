FROM golang:1.24 AS build

WORKDIR /go/src/app
COPY go.mod go.sum ./
RUN go mod download && go mod verify
#RUN go vet -v

COPY *.go ./

#RUN go vet -v
#RUN go test -v $(go list ./... | grep -v /e2e)

RUN go build -o /go/bin/app

FROM gcr.io/distroless/base

COPY --from=build /go/bin/app /
CMD ["/app"]
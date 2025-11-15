FROM golang:1.24 as build

WORKDIR /work

COPY go.* ./
RUN go mod download

COPY main.go main.go
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o gateway-yeeter main.go

FROM gcr.io/distroless/static:nonroot

WORKDIR /
COPY --from=build /work/gateway-yeeter .

USER 65532:65532

ENTRYPOINT ["/gateway-yeeter"]
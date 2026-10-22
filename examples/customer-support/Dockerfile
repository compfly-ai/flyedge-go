FROM golang:1.25 AS build
WORKDIR /src
COPY . .
WORKDIR /src/examples/customer-support
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -p 2 -o /customer-support-go .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /customer-support-go /customer-support-go
ENV AGENT_ADDR=:8096
EXPOSE 8096
ENTRYPOINT ["/customer-support-go"]

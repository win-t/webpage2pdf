#syntax=docker/dockerfile:1

FROM golang:latest AS builder

ENV GOCACHE=/cache/GOCACHE
ENV GOMODCACHE=/cache/GOMODCACHE

WORKDIR /workdir

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/cache go mod download

COPY . ./
RUN --mount=type=cache,target=/cache go build -o /output/bootstrap -trimpath -tags lambda.norpc .


FROM chromedp/headless-shell:latest AS app

COPY --from=builder /output/bootstrap /

USER nobody
ENTRYPOINT ["/bootstrap"]

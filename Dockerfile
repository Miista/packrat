# Used by `make docker`: the binary is prebuilt on the host and copied in,
# rather than built inside Docker.
FROM alpine:3 AS certs
RUN apk add --no-cache ca-certificates

FROM scratch
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY packrat /packrat
COPY web/static /web/static
WORKDIR /
EXPOSE 8766
ENTRYPOINT ["/packrat"]

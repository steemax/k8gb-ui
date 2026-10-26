FROM --platform=$BUILDPLATFORM golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af AS build
WORKDIR /src
ENV GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG COMPONENT=console
ARG TARGETOS
ARG TARGETARCH
RUN test "$COMPONENT" = console -o "$COMPONENT" = agent -o "$COMPONENT" = webhook
# Keep the symbol table for package-aware binary vulnerability checks.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-w" -o /out/app ./cmd/$COMPONENT
# Retain downloaded dependency license/notice files, including binary redistribution notices.
RUN mkdir -p /out/licenses/dependencies && find /go/pkg/mod -path /go/pkg/mod/cache -prune -o -type f \( -iname 'license*' -o -iname 'notice*' -o -iname 'copying*' -o -name AUTHORS -o -name PATENTS \) -exec cp --parents -t /out/licenses/dependencies {} +
RUN chmod -R u+rwX,go+rX /out/licenses

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG VERSION=dev
LABEL org.opencontainers.image.title="k8gb-ui" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/app /app
COPY --from=build /out/licenses /licenses
COPY --from=build /usr/local/go/LICENSE /licenses/Go.LICENSE
COPY LICENSE /licenses/k8gb-ui.LICENSE
COPY web/static/vendor/lucide.LICENSE /licenses/lucide.LICENSE
EXPOSE 8080 8081 8443
ENTRYPOINT ["/app"]
